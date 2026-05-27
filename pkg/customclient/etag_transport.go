package customclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/maypok86/otter/v2"
	"go.uber.org/zap"
)

const (
	defaultETagCacheMaxMB = 100
	defaultETagCacheTTL   = 24 * time.Hour
	etagHitHeader         = "X-Baton-Etag-Cache"
	etagHitHeaderValue    = "HIT"
	authHashCacheKeyBytes = 8
)

// etagCacheEntry holds the cached body and headers from a 200 response.
// Used to satisfy subsequent 304 Not Modified responses without paying
// rate-limit cost. Per GitHub's REST best-practices documentation, 304
// responses to If-None-Match requests do not count against the primary
// rate-limit budget.
type etagCacheEntry struct {
	ETag         string
	LastModified string
	Body         []byte
	ContentType  string
	StoredAt     time.Time
}

// ETagCacheStats reports cumulative cache effectiveness for the transport's
// lifetime. Useful for one-line "ratio = N%" log lines at sync end.
type ETagCacheStats struct {
	Hits304  uint64
	Misses   uint64
	Stores   uint64
	Bypassed uint64
}

// ETagTransport wraps an http.RoundTripper to perform GitHub-style conditional
// requests: it sends If-None-Match on GETs for which we have a cached ETag and
// substitutes the cached body when the server returns 304 Not Modified.
//
// The cache is in-process (Otter LRU, size-capped) and shares lifetime with
// the connector's *http.Client. For Lambda warm starts this carries across
// invocations. For cold starts the cache is empty; Phase 1.5 adds
// session-store-backed persistence.
type ETagTransport struct {
	next     http.RoundTripper
	cache    *otter.Cache[string, *etagCacheEntry]
	authHash string

	hits304  atomic.Uint64
	misses   atomic.Uint64
	stores   atomic.Uint64
	bypassed atomic.Uint64
}

// NewETagTransport constructs the transport. maxMB caps the total cache weight
// (default 100). authToken is hashed (truncated SHA-256) into cache keys so two
// different App installations or PATs running in the same process don't share
// cache entries with each other.
func NewETagTransport(next http.RoundTripper, maxMB int, authToken string) (*ETagTransport, error) {
	if next == nil {
		return nil, fmt.Errorf("etag transport: next round-tripper required")
	}
	if maxMB <= 0 {
		maxMB = defaultETagCacheMaxMB
	}
	capacity := uint64(maxMB) * 1024 * 1024
	cache, err := otter.New(&otter.Options[string, *etagCacheEntry]{
		MaximumWeight: capacity,
		Weigher: func(key string, value *etagCacheEntry) uint32 {
			n := uint64(len(key))
			if value != nil {
				n += uint64(len(value.Body)) + uint64(len(value.ETag)) + uint64(len(value.LastModified)) + uint64(len(value.ContentType))
			}
			if n > uint64(math.MaxUint32) {
				return math.MaxUint32
			}
			return uint32(n)
		},
		ExpiryCalculator: otter.ExpiryWriting[string, *etagCacheEntry](defaultETagCacheTTL),
	})
	if err != nil {
		return nil, fmt.Errorf("etag transport: cache init: %w", err)
	}

	h := sha256.Sum256([]byte(authToken))
	return &ETagTransport{
		next:     next,
		cache:    cache,
		authHash: hex.EncodeToString(h[:authHashCacheKeyBytes]),
	}, nil
}

// Stats returns a snapshot of cumulative counters.
func (t *ETagTransport) Stats() ETagCacheStats {
	return ETagCacheStats{
		Hits304:  t.hits304.Load(),
		Misses:   t.misses.Load(),
		Stores:   t.stores.Load(),
		Bypassed: t.bypassed.Load(),
	}
}

// RoundTrip implements http.RoundTripper. GETs are conditionally cached; all
// other methods pass through.
func (t *ETagTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only GETs benefit from conditional requests. Defensive: also bypass when
	// the caller already set If-None-Match (don't overwrite their choice).
	if req.Method != http.MethodGet || req.Header.Get("If-None-Match") != "" {
		t.bypassed.Add(1)
		return t.next.RoundTrip(req)
	}

	key := t.cacheKey(req)
	entry, present := t.cache.GetIfPresent(key)
	hasUsableEntry := present && entry != nil && entry.ETag != ""

	if hasUsableEntry {
		// Clone to avoid mutating the caller's request (go-github may retry).
		req = req.Clone(req.Context())
		req.Header.Set("If-None-Match", entry.ETag)
		if entry.LastModified != "" {
			req.Header.Set("If-Modified-Since", entry.LastModified)
		}
	}

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	if resp.StatusCode == http.StatusNotModified {
		if hasUsableEntry {
			t.hits304.Add(1)
			return t.materializeFromCache(resp, entry), nil
		}
		// Server returned 304 but we have no cached body to substitute. This
		// can happen if the entry was evicted between sending the request and
		// receiving the response, or if some intermediary spoofed an
		// If-None-Match header. Drain and retry without conditional headers so
		// the caller sees real data rather than an unparseable 304.
		_ = resp.Body.Close()
		ctxzap.Extract(req.Context()).Warn(
			"baton-github: http-etag-cache got 304 without cached entry; retrying unconditional",
			zap.String("url", req.URL.String()),
		)
		retry := req.Clone(req.Context())
		retry.Header.Del("If-None-Match")
		retry.Header.Del("If-Modified-Since")
		return t.next.RoundTrip(retry)
	}

	// Persist responses with an ETag so subsequent requests can short-circuit.
	if resp.StatusCode == http.StatusOK {
		respETag := resp.Header.Get("ETag")
		if respETag != "" {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			t.cache.Set(key, &etagCacheEntry{
				ETag:         respETag,
				LastModified: resp.Header.Get("Last-Modified"),
				Body:         body,
				ContentType:  resp.Header.Get("Content-Type"),
				StoredAt:     time.Now(),
			})
			t.stores.Add(1)
		} else {
			t.misses.Add(1)
		}
		return resp, nil
	}

	t.misses.Add(1)
	return resp, nil
}

// materializeFromCache returns a synthetic 200 response with the cached body,
// preserving the 304 response's headers (notably rate-limit headers) so the
// connector's rate-limit accounting stays accurate.
func (t *ETagTransport) materializeFromCache(resp304 *http.Response, entry *etagCacheEntry) *http.Response {
	_ = resp304.Body.Close()
	out := &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         resp304.Proto,
		ProtoMajor:    resp304.ProtoMajor,
		ProtoMinor:    resp304.ProtoMinor,
		Header:        cloneHeaders(resp304.Header),
		Body:          io.NopCloser(bytes.NewReader(entry.Body)),
		ContentLength: int64(len(entry.Body)),
		Request:       resp304.Request,
		TLS:           resp304.TLS,
	}
	if entry.ContentType != "" {
		out.Header.Set("Content-Type", entry.ContentType)
	}
	out.Header.Set("ETag", entry.ETag)
	if entry.LastModified != "" {
		out.Header.Set("Last-Modified", entry.LastModified)
	}
	out.Header.Set(etagHitHeader, etagHitHeaderValue)
	return out
}

func cloneHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		vv := make([]string, len(v))
		copy(vv, v)
		out[k] = vv
	}
	return out
}

// cacheKey derives a stable, collision-resistant key from method + host + path
// + sorted query params, prefixed by the auth-scope hash so installations are
// isolated.
func (t *ETagTransport) cacheKey(req *http.Request) string {
	h := sha256.New()
	_, _ = h.Write([]byte(t.authHash))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(req.URL.Host))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(req.URL.Path))
	_, _ = h.Write([]byte{0})

	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0})
		for _, v := range q[k] {
			_, _ = h.Write([]byte(v))
			_, _ = h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// LogStats writes a structured summary at the caller's chosen log point
// (typically sync end or per-builder). Greppable as "http-etag-cache".
func (t *ETagTransport) LogStats(ctx context.Context) {
	s := t.Stats()
	total := s.Hits304 + s.Misses
	ratio := 0.0
	if total > 0 {
		ratio = 100.0 * float64(s.Hits304) / float64(total)
	}
	ctxzap.Extract(ctx).Info("http-etag-cache",
		zap.Uint64("hits_304", s.Hits304),
		zap.Uint64("misses", s.Misses),
		zap.Uint64("stores", s.Stores),
		zap.Uint64("bypassed_non_get", s.Bypassed),
		zap.Float64("hit_ratio_pct", ratio),
	)
}
