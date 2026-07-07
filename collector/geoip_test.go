package main

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
)

// The collector must boot and decode to empty geo when no database is
// configured — geo is enrichment, never a gate on ingest.
func TestGeoResolverBestEffort(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := map[string]*geoResolver{
		"nil resolver":    nil,
		"unset path":      newGeoResolver("", log),
		"missing db file": newGeoResolver("does-not-exist.mmdb", log),
	}
	for name, g := range cases {
		got := g.resolve("8.8.8.8")
		if got != (geoResult{}) {
			t.Errorf("%s: expected empty geoResult, got %+v", name, got)
		}
	}

	// Even with no DB, an empty/garbage IP must not panic.
	g := newGeoResolver("", log)
	if got := g.resolve(""); got != (geoResult{}) {
		t.Errorf("empty ip: got %+v", got)
	}
	if got := g.resolve("not-an-ip"); got != (geoResult{}) {
		t.Errorf("garbage ip: got %+v", got)
	}
}

// buildProto must surface every enricher geo field onto the wire message.
func TestBuildProtoGeoPassthrough(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ev := &Event{EventID: "e_1", EventName: "page_viewed", AnonymousID: "a_1", SessionID: "s_1"}
	enr := newEnricher(httptest.NewRequest("POST", "/v1/events", nil), "demo-app", &geoResolver{})
	enr.Country, enr.Region, enr.City = "IN", "Karnataka", "Bengaluru"
	enr.Lat, enr.Lon = "12.9634", "77.5855"

	pb := buildProto(ev, enr, log)
	if pb.GetGeoCountry() != "IN" || pb.GetGeoRegion() != "Karnataka" ||
		pb.GetGeoCity() != "Bengaluru" || pb.GetGeoLat() != "12.9634" || pb.GetGeoLon() != "77.5855" {
		t.Errorf("geo fields not passed through: country=%q region=%q city=%q lat=%q lon=%q",
			pb.GetGeoCountry(), pb.GetGeoRegion(), pb.GetGeoCity(), pb.GetGeoLat(), pb.GetGeoLon())
	}
}
