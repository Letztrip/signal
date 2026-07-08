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
	Now      time.Time
	AppID    string
	UA       useragent.UserAgent
	UAFamily string
	Country  string
	Region   string
	City     string
	Lat      string
	Lon      string
}

func newEnricher(r *http.Request, appID string, geo *geoResolver) *enricher {
	rawUA := r.Header.Get("User-Agent")
	ua := useragent.Parse(rawUA)
	// Cloud Run injects the resolved client IP into X-Forwarded-For; geo is
	// best-effort — a nil/empty resolver yields empty location fields.
	loc := geo.resolve(clientIP(r))
	return &enricher{
		Now:      time.Now().UTC(),
		AppID:    appID,
		UA:       ua,
		UAFamily: uaFamily(ua, rawUA),
		Country:  loc.Country,
		Region:   loc.Region,
		City:     loc.City,
		Lat:      loc.Lat,
		Lon:      loc.Lon,
	}
}

// uaFamily is the value stored in the ua_family column and matched by the
// analytics bot filter (pulse-service SignalQueries.botPredicate). It's the
// parsed browser/bot name, but when the UA parser flags a crawler it couldn't
// name (ua.Name == ""), we stamp "bot" so those rows are still excluded from
// human traffic — otherwise unnamed AI crawlers (GPTBot, ClaudeBot, …) leak in
// as "human". Never mark the Flutter "Dart" HTTP runtime: it is real mobile
// users, and the bot filter must never match it.
func uaFamily(ua useragent.UserAgent, rawUA string) string {
	if ua.Name != "" {
		return ua.Name
	}
	if ua.Bot && !strings.Contains(strings.ToLower(rawUA), "dart") {
		return "bot"
	}
	return ""
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
