package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSPreflight_AllowAll(t *testing.T) {
	mw := corsMiddleware([]string{"*"})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("preflight should short-circuit; next handler ran")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/v1/events", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "X-Write-Key, Idempotency-Key, Content-Type")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin: got %q want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Access-Control-Allow-Methods missing POST: %q", got)
	}
	for _, h := range []string{"X-Write-Key", "Idempotency-Key", "Content-Type"} {
		if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, h) {
			t.Errorf("Access-Control-Allow-Headers missing %s: %q", h, got)
		}
	}
}

func TestCORSPreflight_WhitelistMatch(t *testing.T) {
	mw := corsMiddleware([]string{"https://app.example.com", "https://admin.example.com"})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("preflight should short-circuit")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/v1/events", nil)
	req.Header.Set("Origin", "https://admin.example.com")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
		t.Errorf("Access-Control-Allow-Origin: got %q want https://admin.example.com", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary header should include Origin, got %q", got)
	}
}

func TestCORSPreflight_WhitelistMiss(t *testing.T) {
	mw := corsMiddleware([]string{"https://app.example.com"})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("preflight should short-circuit")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/v1/events", nil)
	req.Header.Set("Origin", "https://evil.example.com")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin should be empty for non-whitelisted origin, got %q", got)
	}
}

func TestCORS_ActualPOST_AddsAllowOriginHeader(t *testing.T) {
	mw := corsMiddleware([]string{"*"})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://app.example.com")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("POST should flow through to the next handler")
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("status: got %d want 202 (handler's response)", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin on real POST: got %q want *", got)
	}
}

func TestCORS_NoOriginHeader_NoCORSHeaders(t *testing.T) {
	mw := corsMiddleware([]string{"*"})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	// no Origin header — same-origin request
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("non-CORS request should flow through")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO should be empty for non-CORS requests, got %q", got)
	}
}
