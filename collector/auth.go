package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

// Argon2id parameter floor. RFC 9106 lighter profile, sized so that one
// authenticated request costs a few ms on Cloud Run's smallest CPU shape.
// Hashes encoded with anything weaker than these values are rejected at
// load time — operators upgrading to stronger params just rotate the
// Secret Manager version and the next 5-minute refresh picks them up.
const (
	argonMemoryKiB  uint32 = 64 * 1024 // 64 MiB
	argonTimeCost   uint32 = 3
	argonThreads    uint8  = 2
	argonHashLength uint32 = 32

	keyRefreshInterval = 5 * time.Minute
)

type ctxKey int

const (
	ctxKeyAppID ctxKey = iota
	ctxKeyRequestID
)

// keyEntry is one parsed secret line: an app id and the argon2id verifier.
type keyEntry struct {
	appID   string
	memKiB  uint32
	time    uint32
	threads uint8
	salt    []byte
	hash    []byte
}

// KeyStore is an immutable snapshot of the parsed secret. We swap in new
// snapshots atomically when Secret Manager rotates.
type KeyStore struct {
	entries []keyEntry
}

// Verify walks every entry and runs argon2id over the candidate, returning
// the matched app_id on success. We always run all entries so verification
// time is independent of which key matches (and of whether any key matches).
func (ks *KeyStore) Verify(candidate string) (string, bool) {
	if ks == nil || candidate == "" || len(ks.entries) == 0 {
		return "", false
	}
	candBytes := []byte(candidate)
	matchedApp := ""
	for _, e := range ks.entries {
		got := argon2.IDKey(candBytes, e.salt, e.time, e.memKiB, e.threads, uint32(len(e.hash)))
		if subtle.ConstantTimeCompare(got, e.hash) == 1 {
			matchedApp = e.appID
		}
	}
	if matchedApp == "" {
		return "", false
	}
	return matchedApp, true
}

// ParseKeyStorePlaintext loads keys from an inline spec of the form:
//
//	<app-id>:<plaintext-key>
//
// Entries may be separated by newline or comma. Lines that are blank or
// start with '#' are ignored. Plaintext keys are hashed at parse time, so
// the runtime verify path stays argon2id-based — same constant-time
// comparison as the Secret Manager path. Intended for local dev only;
// production should use Secret Manager.
func ParseKeyStorePlaintext(raw string) (*KeyStore, error) {
	ks := &KeyStore{}
	normalized := strings.ReplaceAll(raw, ",", "\n")
	for ln, line := range strings.Split(normalized, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		appID, plaintext, ok := strings.Cut(line, ":")
		if !ok || appID == "" || plaintext == "" {
			return nil, fmt.Errorf("entry %d: expected '<app-id>:<plaintext-key>'", ln+1)
		}
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, fmt.Errorf("salt: %w", err)
		}
		hash := argon2.IDKey([]byte(strings.TrimSpace(plaintext)), salt,
			argonTimeCost, argonMemoryKiB, argonThreads, argonHashLength)
		ks.entries = append(ks.entries, keyEntry{
			appID:   strings.TrimSpace(appID),
			memKiB:  argonMemoryKiB,
			time:    argonTimeCost,
			threads: argonThreads,
			salt:    salt,
			hash:    hash,
		})
	}
	if len(ks.entries) == 0 {
		return nil, errors.New("no usable keys parsed from WRITE_KEYS_PLAINTEXT")
	}
	return ks, nil
}

// ParseKeyStore parses one-line-per-key secret content of the form:
//
//	<app-id>:<phc-encoded-argon2id-hash>
//
// PHC string layout: $argon2id$v=19$m=65536,t=3,p=2$<salt-b64>$<hash-b64>
//
// Lines that are blank or start with '#' are ignored.
func ParseKeyStore(raw string) (*KeyStore, error) {
	ks := &KeyStore{}
	scanner := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	for ln, line := range scanner {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		appID, phc, ok := strings.Cut(line, ":")
		if !ok || appID == "" || phc == "" {
			return nil, fmt.Errorf("line %d: expected '<app-id>:<phc-hash>'", ln+1)
		}
		entry, err := parsePHC(strings.TrimSpace(appID), strings.TrimSpace(phc))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ln+1, err)
		}
		ks.entries = append(ks.entries, entry)
	}
	if len(ks.entries) == 0 {
		return nil, errors.New("no usable keys parsed from secret")
	}
	return ks, nil
}

// parsePHC accepts only argon2id PHC strings.
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt-b64>$<hash-b64>
func parsePHC(appID, phc string) (keyEntry, error) {
	parts := strings.Split(phc, "$")
	// Expected layout: ["", "argon2id", "v=19", "m=...,t=...,p=...", "salt", "hash"]
	if len(parts) != 6 || parts[0] != "" {
		return keyEntry{}, fmt.Errorf("malformed PHC string")
	}
	if parts[1] != "argon2id" {
		return keyEntry{}, fmt.Errorf("unsupported algorithm %q (need argon2id)", parts[1])
	}
	if parts[2] != "v=19" {
		return keyEntry{}, fmt.Errorf("unsupported argon2 version %q", parts[2])
	}

	mem, t, p, err := parseArgonParams(parts[3])
	if err != nil {
		return keyEntry{}, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return keyEntry{}, fmt.Errorf("decode salt: %w", err)
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return keyEntry{}, fmt.Errorf("decode hash: %w", err)
	}
	if mem < argonMemoryKiB || t < argonTimeCost || p < argonThreads || uint32(len(hash)) < argonHashLength {
		return keyEntry{}, fmt.Errorf("argon2 params below floor (m=%d t=%d p=%d hash=%d); minimums m=%d t=%d p=%d hash=%d",
			mem, t, p, len(hash), argonMemoryKiB, argonTimeCost, argonThreads, argonHashLength)
	}
	return keyEntry{
		appID:   appID,
		memKiB:  mem,
		time:    t,
		threads: p,
		salt:    salt,
		hash:    hash,
	}, nil
}

func parseArgonParams(s string) (mem, t uint32, p uint8, err error) {
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return 0, 0, 0, fmt.Errorf("malformed param %q", kv)
		}
		switch k {
		case "m":
			n, perr := strconv.ParseUint(v, 10, 32)
			if perr != nil {
				return 0, 0, 0, fmt.Errorf("m: %w", perr)
			}
			mem = uint32(n)
		case "t":
			n, perr := strconv.ParseUint(v, 10, 32)
			if perr != nil {
				return 0, 0, 0, fmt.Errorf("t: %w", perr)
			}
			t = uint32(n)
		case "p":
			n, perr := strconv.ParseUint(v, 10, 8)
			if perr != nil {
				return 0, 0, 0, fmt.Errorf("p: %w", perr)
			}
			p = uint8(n)
		default:
			return 0, 0, 0, fmt.Errorf("unknown param %q", k)
		}
	}
	if mem == 0 || t == 0 || p == 0 {
		return 0, 0, 0, fmt.Errorf("missing m/t/p in %q", s)
	}
	return mem, t, p, nil
}

// KeyManager wraps an atomic snapshot of the KeyStore and the Secret Manager
// resource name to refresh from. The pointer is what handlers read on the
// hot path; refreshes swap it atomically.
type KeyManager struct {
	current  atomic.Pointer[KeyStore]
	resource string
	log      *slog.Logger
}

// NewKeyManager loads the secret synchronously once so the collector fails
// fast on misconfiguration.
func NewKeyManager(ctx context.Context, resource string, log *slog.Logger) (*KeyManager, error) {
	if resource == "" {
		return nil, errors.New("WRITE_KEYS_SECRET is required")
	}
	km := &KeyManager{resource: resource, log: log}
	ks, err := km.fetchAndParse(ctx)
	if err != nil {
		return nil, fmt.Errorf("initial secret load: %w", err)
	}
	km.current.Store(ks)
	return km, nil
}

// NewKeyManagerStatic loads keys from an inline plaintext spec and disables
// the refresh loop (no resource to refresh from). Local dev only — surfaces
// a loud warning so this never sneaks into production.
func NewKeyManagerStatic(raw string, log *slog.Logger) (*KeyManager, error) {
	ks, err := ParseKeyStorePlaintext(raw)
	if err != nil {
		return nil, err
	}
	log.Warn("write-keys loaded from WRITE_KEYS_PLAINTEXT — DEV MODE; use Secret Manager in production",
		"count", len(ks.entries))
	km := &KeyManager{log: log}
	km.current.Store(ks)
	return km, nil
}

// Run blocks until ctx is cancelled. When backed by Secret Manager it
// refreshes every 5 minutes; in static mode it just blocks.
func (km *KeyManager) Run(ctx context.Context) {
	if km.resource == "" {
		<-ctx.Done()
		return
	}
	t := time.NewTicker(keyRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ks, err := km.fetchAndParse(ctx)
			if err != nil {
				km.log.Warn("write-keys refresh failed", "err", err)
				continue
			}
			km.current.Store(ks)
			km.log.Info("write-keys refreshed", "count", len(ks.entries))
		}
	}
}

func (km *KeyManager) Snapshot() *KeyStore {
	return km.current.Load()
}

func (km *KeyManager) fetchAndParse(ctx context.Context) (*KeyStore, error) {
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("secretmanager client: %w", err)
	}
	defer client.Close()

	resp, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: km.resource,
	})
	if err != nil {
		return nil, fmt.Errorf("access secret: %w", err)
	}
	return ParseKeyStore(string(resp.GetPayload().GetData()))
}

func appIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyAppID).(string); ok {
		return v
	}
	return ""
}

func requestIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", rid)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func recoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic", "err", rec, "path", r.URL.Path)
					writeErr(w, http.StatusInternalServerError, "internal", "internal error", requestIDFromCtx(r.Context()))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			log.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", requestIDFromCtx(r.Context()),
			)
		})
	}
}

func authMiddleware(km *KeyManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			candidate := r.Header.Get("X-Write-Key")
			app, ok := km.Snapshot().Verify(candidate)
			if !ok {
				writeErr(w, http.StatusUnauthorized, "invalid_write_key", "missing or invalid X-Write-Key", requestIDFromCtx(r.Context()))
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeyAppID, app)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
