package main

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mileusna/useragent"
)

const ingestVersion = "v1"

type enricher struct {
	Now     time.Time
	AppID   string
	UA      useragent.UserAgent
	Country string
}

func newEnricher(r *http.Request, appID string) *enricher {
	ua := useragent.Parse(r.Header.Get("User-Agent"))
	return &enricher{
		Now:   time.Now().UTC(),
		AppID: appID,
		UA:    ua,
		// Real GeoIP would resolve clientIP(r) against MaxMind / a sidecar
		// here. Cloud Run injects the resolved client IP into
		// X-Forwarded-For; for now we leave geo_country empty.
		Country: "",
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
