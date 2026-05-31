package main

// Observability: Sentry (errors + tracing) and a Bearer-authed Prometheus
// /metrics endpoint, wired per the letztrip-observability fleet conventions.
//
// This reverses the repo's original "no Prometheus" stance (see CLAUDE.md) so
// signal shows up in the shared VictoriaMetrics/Grafana alongside the other
// services. Everything here is isolated and best-effort: a dead Sentry or
// monitoring VM must never fail an ingest request.

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const sentryFlushTimeout = 2 * time.Second

// metricsRegistry is a dedicated registry so /metrics exposes exactly the
// signal_* + go_* + process_* series and nothing incidental.
var metricsRegistry = prometheus.NewRegistry()

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "signal_http_requests_total",
			Help: "HTTP requests handled, by method, matched route, and status.",
		},
		[]string{"method", "route", "status"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "signal_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"method", "route"},
	)
	eventsReceivedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "signal_events_received_total",
			Help: "Individual events accepted and published across all batches.",
		},
	)
	eventsPublishedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "signal_events_published_total",
			Help: "Pub/Sub publish outcomes.",
		},
		[]string{"result"}, // ok|error
	)
	piiRejectedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "signal_pii_rejected_total",
			Help: "Event batches rejected because a property matched the PII scanner.",
		},
	)
	buildInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "signal_build_info",
			Help: "Build/deploy info; value is always 1.",
		},
		[]string{"version"},
	)
)

func init() {
	metricsRegistry.MustRegister(
		httpRequestsTotal,
		httpRequestDuration,
		eventsReceivedTotal,
		eventsPublishedTotal,
		piiRejectedTotal,
		buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	buildInfo.WithLabelValues(envOr("COLLECTOR_VERSION", "unknown")).Set(1)
}

// isProduction reports whether ENVIRONMENT names production, case-insensitively
// (this service deploys with ENVIRONMENT=production, lowercase).
func isProduction() bool {
	return strings.EqualFold(os.Getenv("ENVIRONMENT"), "production")
}

// initSentry wires Sentry for errors + tracing. No-op (returns false) unless
// ENVIRONMENT=production and SENTRY_DSN is set, so dev/test stay silent. Never
// fails the service: on init error it logs and returns false.
func initSentry(log *slog.Logger) bool {
	dsn := strings.TrimSpace(os.Getenv("SENTRY_DSN"))
	if !isProduction() || dsn == "" {
		log.Info("sentry disabled (needs ENVIRONMENT=production and SENTRY_DSN)")
		return false
	}
	release := os.Getenv("SENTRY_RELEASE")
	if release == "" {
		release = os.Getenv("COLLECTOR_VERSION")
	}
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      os.Getenv("ENVIRONMENT"),
		Release:          release,
		EnableTracing:    true,
		TracesSampleRate: envFloat("SENTRY_TRACES_SAMPLE_RATE", 1.0),
		SendDefaultPII:   false,
	}); err != nil {
		log.Warn("sentry init failed; continuing without it", "err", err)
		return false
	}
	log.Info("sentry initialised")
	return true
}

// sentryMiddleware reports panics to Sentry and opens a per-request hub /
// transaction. Repanic is true so the existing recoverMiddleware still writes
// the 500 response.
func sentryMiddleware() func(http.Handler) http.Handler {
	return sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle
}

// captureErr sends a handled (non-panic) error to Sentry via the request's hub.
// No-op when Sentry is disabled (the hub has no client).
func captureErr(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if hub := sentry.GetHubFromContext(ctx); hub != nil {
		hub.CaptureException(err)
	}
}

// metricsMiddleware records per-request count + duration, labelled by the chi
// route pattern (bounded cardinality; unmatched paths collapse to "unmatched").
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil {
			if p := rc.RoutePattern(); p != "" {
				route = p
			}
		}
		httpRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		httpRequestDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

// metricsHandler serves the Prometheus registry, Bearer-authenticated with
// METRICS_AUTH_TOKEN (constant-time compare). When no token is configured it
// refuses in production (503) and serves openly elsewhere (local dev) —
// matching aaru / letztrip-backend.
func metricsHandler(token string) http.Handler {
	token = strings.TrimSpace(token)
	tokenBytes := []byte(token)
	production := isProduction()
	prom := promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			if production {
				http.Error(w, "metrics endpoint not configured", http.StatusServiceUnavailable)
				return
			}
			prom.ServeHTTP(w, r)
			return
		}
		provided := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			provided = strings.TrimSpace(h[len("Bearer "):])
		}
		if provided == "" || subtle.ConstantTimeCompare(tokenBytes, []byte(provided)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		prom.ServeHTTP(w, r)
	})
}

func envFloat(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
