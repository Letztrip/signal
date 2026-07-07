package main

import (
	"log/slog"
	"net"
	"strconv"

	"github.com/oschwald/geoip2-golang"
)

// geoResolver decodes a client IP into coarse location fields using a MaxMind
// GeoLite2-City database. It is best-effort by design: an unconfigured or
// unopenable database yields a resolver that decodes to empty geo rather than
// an error. Geo is enrichment, never a gate on ingest — the collector must
// still boot and accept events when the .mmdb is absent (local dev, tests, or
// a first deploy before the file is committed).
type geoResolver struct {
	city *geoip2.Reader
}

// geoResult holds the decoded fields. Lat/Lon are stringified to match the
// all-string wire contract (see schemas/event.proto); empty when unknown.
type geoResult struct {
	Country string // ISO 3166-1 alpha-2, e.g. "IN"
	Region  string // most-specific subdivision name, e.g. "Karnataka"
	City    string
	Lat     string
	Lon     string
}

// newGeoResolver opens the GeoLite2-City database at path. An empty path or an
// open failure logs a warning and returns a resolver with no database, so
// resolve() degrades to empty geo. It never returns an error — the caller
// wires it unconditionally and the service boots either way.
func newGeoResolver(path string, log *slog.Logger) *geoResolver {
	if path == "" {
		log.Warn("geoip disabled", "reason", "GEOIP_DB_PATH not set")
		return &geoResolver{}
	}
	db, err := geoip2.Open(path)
	if err != nil {
		log.Warn("geoip disabled", "reason", "open failed", "path", path, "err", err)
		return &geoResolver{}
	}
	log.Info("geoip enabled", "path", path)
	return &geoResolver{city: db}
}

// Close releases the underlying database handle. Nil-safe.
func (g *geoResolver) Close() error {
	if g == nil || g.city == nil {
		return nil
	}
	return g.city.Close()
}

// resolve decodes ip into a geoResult. A nil resolver, missing database,
// empty/unparseable IP, or lookup miss all return the zero value — never an
// error. Coordinates of exactly (0,0) are treated as "unknown" (Null Island).
func (g *geoResolver) resolve(ip string) geoResult {
	if g == nil || g.city == nil || ip == "" {
		return geoResult{}
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return geoResult{}
	}
	rec, err := g.city.City(parsed)
	if err != nil {
		return geoResult{}
	}
	res := geoResult{
		Country: rec.Country.IsoCode,
		City:    rec.City.Names["en"],
	}
	if len(rec.Subdivisions) > 0 {
		res.Region = rec.Subdivisions[0].Names["en"]
	}
	if rec.Location.Latitude != 0 || rec.Location.Longitude != 0 {
		res.Lat = strconv.FormatFloat(rec.Location.Latitude, 'f', -1, 64)
		res.Lon = strconv.FormatFloat(rec.Location.Longitude, 'f', -1, 64)
	}
	return res
}
