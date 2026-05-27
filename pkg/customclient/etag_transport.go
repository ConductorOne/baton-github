package customclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/conductorone/baton-sdk/pkg/types/sessions"
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
// lifetime. Useful for one-line "ratio = N%" log lines at sync end. The
// persistence-related counters (OversizedSkipped, DroppedWrites, Persisted)
// are zero when no session store is wired in.
type ETagCacheStats struct {
	Hits304          uint64
	Misses           uint64
	Stores           uint64
	Bypassed         uint64
	OversizedSkipped uint64
	DroppedWrites    uint64
	Persisted        uint64
}

// saveJob carries a single entry from RoundTrip's hot path to the background
// writer goroutine that talks to the persistor.
type saveJob struct {
	key   string
	entry *etagCacheEntry
}

const (
	// saveQueueDepth bounds the writer goroutine's buffer. A 5,000-repo sync
	// with collaborator + team lists per repo emits roughly 10–20k cacheable
	// responses; 256 keeps memory bounded while absorbing burst writes from
	// concurrent SDK pool workers.
	saveQueueDepth = 256
	// drainTimeout caps how long Close waits for in-flight saves to drain
	// before forcing shutdown.
	drainTimeout = 5 * time.Second
	// saveOpTimeout caps any single Save call inside the writer goroutine.
	saveOpTimeout = 2 * time.Second
)

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

	hits304          atomic.Uint64
	misses           atomic.Uint64
	stores           atomic.Uint64
	bypassed         atomic.Uint64
	oversizedSkipped atomic.Uint64
	droppedWrites    atomic.Uint64
	persisted        atomic.Uint64

	// persistor is set to noopPersistor by NewETagTransport and may be swapped
	// once by SetSession to a sessionPersistor backed by the SDK session store.
	// Access via persistorPtr.Load(); writes are serialized by sessionOnce.
	persistorPtr atomic.Pointer[Persistor]
	sessionOnce  sync.Once

	// saveCh feeds the writer goroutine; non-blocking sends from RoundTrip.
	saveCh chan saveJob
	// closeOnce gates Close so multiple callers see consistent behavior.
	closeOnce sync.Once
	// closed is set before saveCh is closed so late-arriving enqueueSave callers
	// can short-circuit instead of panicking on send-to-closed-channel. The
	// SDK typically calls Close after sync ends, but the contract doesn't
	// guarantee no RoundTrips are in flight.
	closed atomic.Bool
	// writerDone signals the writer goroutine has exited.
	writerDone chan struct{}
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
	t := &ETagTransport{
		next:       next,
		cache:      cache,
		authHash:   hex.EncodeToString(h[:authHashCacheKeyBytes]),
		saveCh:     make(chan saveJob, saveQueueDepth),
		writerDone: make(chan struct{}),
	}
	var p Persistor = noopPersistor{}
	t.persistorPtr.Store(&p)
	go t.runWriter()
	return t, nil
}

// SetSession wires in the SDK session store as the persistence backend. Safe
// to call concurrently; only the first call wins (subsequent calls are no-ops
// so re-binding can't accidentally clobber an active cache hydrate). The
// initial LoadAll runs in a goroutine so SetSession returns immediately —
// requests issued before the load completes simply see cache misses, which is
// correct (and the same as Phase 1's behavior). Logs warnings on load errors
// rather than failing; persistence is best-effort.
func (t *ETagTransport) SetSession(ctx context.Context, ss sessions.SessionStore) {
	if ss == nil {
		return
	}
	t.sessionOnce.Do(func() {
		var p Persistor = newSessionPersistor(ss, t.authHash)
		t.persistorPtr.Store(&p)
		go t.hydrateFromPersistor(ctx, p)
	})
}

// hydrateFromPersistor populates the in-memory Otter cache from the prior
// sync's persisted entries. Best-effort: errors are logged and the cache
// simply remains empty.
//
// Hydrate runs in a background goroutine spawned by SetSession, so by the
// time it executes, in-flight RoundTrips may already have populated the cache
// with fresher entries fetched during the current sync. Skip those — never
// clobber a newer in-process entry with a 7-day-old persisted one.
func (t *ETagTransport) hydrateFromPersistor(ctx context.Context, p Persistor) {
	entries, err := p.LoadAll(ctx)
	if err != nil {
		ctxzap.Extract(ctx).Warn("baton-github: http-etag-cache hydrate failed",
			zap.Error(err),
		)
		return
	}
	hydrated := 0
	for key, entry := range entries {
		if entry == nil {
			continue
		}
		if existing, ok := t.cache.GetIfPresent(key); ok && existing != nil && !existing.StoredAt.Before(entry.StoredAt) {
			// In-process entry is at least as fresh; leave it alone.
			continue
		}
		t.cache.Set(key, entry)
		hydrated++
	}
	ctxzap.Extract(ctx).Info("baton-github: http-etag-cache hydrated",
		zap.Int("entries_loaded", len(entries)),
		zap.Int("entries_installed", hydrated),
	)
}

// runWriter drains saveCh, persisting each entry. Best-effort: errors are
// logged and counters incremented; we never retry or block the producer.
func (t *ETagTransport) runWriter() {
	defer close(t.writerDone)
	for job := range t.saveCh {
		pp := t.persistorPtr.Load()
		if pp == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), saveOpTimeout)
		err := (*pp).Save(ctx, job.key, job.entry)
		cancel()
		if err != nil {
			// Persistence is best-effort; don't propagate. Log at Warn so
			// operators can see chronic failures without flooding Error logs.
			ctxzap.Extract(ctx).Warn("baton-github: http-etag-cache save failed",
				zap.String("key", job.key),
				zap.Error(err),
			)
			continue
		}
		t.persisted.Add(1)
	}
}

// Close drains pending writes (bounded by drainTimeout) so we don't lose
// in-flight persistence updates on shutdown. Idempotent.
func (t *ETagTransport) Close(ctx context.Context) error {
	var closeErr error
	t.closeOnce.Do(func() {
		// Mark closed BEFORE closing the channel so any concurrent
		// enqueueSave callers see the flag and short-circuit. Without this
		// guard, a select-default on a closed channel proceeds to the send
		// case and panics — select treats send-to-closed as ready, not
		// blocking, contrary to the intuitive read of the spec.
		t.closed.Store(true)
		close(t.saveCh)
		timer := time.NewTimer(drainTimeout)
		defer timer.Stop()
		select {
		case <-t.writerDone:
			return
		case <-timer.C:
			ctxzap.Extract(ctx).Warn("baton-github: http-etag-cache drain timed out",
				zap.Duration("budget", drainTimeout),
				zap.Uint64("dropped_writes", t.droppedWrites.Load()),
			)
			closeErr = errors.New("etag transport: drain timed out")
		case <-ctx.Done():
			closeErr = ctx.Err()
		}
	})
	return closeErr
}

// Stats returns a snapshot of cumulative counters.
func (t *ETagTransport) Stats() ETagCacheStats {
	return ETagCacheStats{
		Hits304:          t.hits304.Load(),
		Misses:           t.misses.Load(),
		Stores:           t.stores.Load(),
		Bypassed:         t.bypassed.Load(),
		OversizedSkipped: t.oversizedSkipped.Load(),
		DroppedWrites:    t.droppedWrites.Load(),
		Persisted:        t.persisted.Load(),
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
			entry := &etagCacheEntry{
				ETag:         respETag,
				LastModified: resp.Header.Get("Last-Modified"),
				Body:         body,
				ContentType:  resp.Header.Get("Content-Type"),
				StoredAt:     time.Now(),
			}
			t.cache.Set(key, entry)
			t.stores.Add(1)
			t.enqueueSave(key, entry, len(body))
		} else {
			t.misses.Add(1)
		}
		return resp, nil
	}

	t.misses.Add(1)
	return resp, nil
}

// enqueueSave hands a freshly-stored entry to the writer goroutine for
// best-effort persistence. Bodies exceeding persistedMaxEntryBytes are kept in
// the in-process cache (warm reruns still benefit) but skip persistence so we
// don't fill the session store with huge payloads. Sends are non-blocking; a
// full queue is preferable to slowing down the HTTP path.
func (t *ETagTransport) enqueueSave(key string, entry *etagCacheEntry, bodyLen int) {
	if bodyLen > persistedMaxEntryBytes {
		t.oversizedSkipped.Add(1)
		return
	}
	// Guard against send-on-closed-channel panic. The atomic.Bool is set
	// before close(saveCh) inside Close, so any RoundTrip racing with Close
	// observes "closed" and exits without touching the channel. Count the
	// drop so operators can see late writes happening.
	if t.closed.Load() {
		t.droppedWrites.Add(1)
		return
	}
	select {
	case t.saveCh <- saveJob{key: key, entry: entry}:
	default:
		t.droppedWrites.Add(1)
	}
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
	// The 304 response carried Content-Length: 0; rewrite it to match the
	// cached body so any downstream middleware that consults the header sees
	// truthful framing.
	out.Header.Set("Content-Length", strconv.FormatInt(int64(len(entry.Body)), 10))
	// Strip Content-Encoding: cached bodies are stored post-decompression by
	// Go's http.Transport, so claiming "gzip" here would mislead any code that
	// tries to decompress again. (304 responses don't normally carry this,
	// but be defensive.)
	out.Header.Del("Content-Encoding")
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
		zap.Uint64("persisted", s.Persisted),
		zap.Uint64("oversized_skipped", s.OversizedSkipped),
		zap.Uint64("dropped_writes", s.DroppedWrites),
		zap.Float64("hit_ratio_pct", ratio),
	)
}
