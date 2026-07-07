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
	Region  string
	City    string
	Lat     string
	Lon     string
}

func newEnricher(r *http.Request, appID string, geo *geoResolver) *enricher {
	ua := useragent.Parse(r.Header.Get("User-Agent"))
	// Cloud Run injects the resolved client IP into X-Forwarded-For; geo is
	// best-effort — a nil/empty resolver yields empty location fields.
	loc := geo.resolve(clientIP(r))
	return &enricher{
		Now:     time.Now().UTC(),
		AppID:   appID,
		UA:      ua,
		Country: loc.Country,
		Region:  loc.Region,
		City:    loc.City,
		Lat:     loc.Lat,
		Lon:     loc.Lon,
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
