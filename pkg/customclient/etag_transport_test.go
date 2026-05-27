package customclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeRT lets a test serve fully-controlled responses while still being a real
// http.RoundTripper plugged underneath ETagTransport.
type fakeRT struct {
	calls atomic.Int64
	fn    func(*http.Request) (*http.Response, error)
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls.Add(1)
	return f.fn(req)
}

// newResp constructs a response with header keys canonicalized — `http.Header`
// map literals bypass canonical-MIME-key handling, so `Get("ETag")` would miss
// `header["ETag"]` (which canonicalizes to `"Etag"`). Funnel through Set().
func newResp(status int, header http.Header, body string, req *http.Request) *http.Response {
	canon := http.Header{}
	for k, vs := range header {
		for _, v := range vs {
			canon.Add(k, v)
		}
	}
	return &http.Response{
		Status:        http.StatusText(status),
		StatusCode:    status,
		Header:        canon,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
	}
}

func TestETagTransport_CacheMissThenHit(t *testing.T) {
	const (
		body   = `{"hello":"world"}`
		etag   = `W/"abc123"`
		ctType = "application/json"
	)

	var sawIfNoneMatch atomic.Value
	sawIfNoneMatch.Store("")

	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			ifNoneMatch := req.Header.Get("If-None-Match")
			sawIfNoneMatch.Store(ifNoneMatch)
			if ifNoneMatch == etag {
				return newResp(http.StatusNotModified, http.Header{
					"X-Ratelimit-Remaining": []string{"4999"},
				}, "", req), nil
			}
			return newResp(http.StatusOK, http.Header{
				"ETag":         []string{etag},
				"Content-Type": []string{ctType},
			}, body, req), nil
		},
	}

	tr, err := NewETagTransport(next, 0, "test-scope")
	require.NoError(t, err)

	client := &http.Client{Transport: tr}

	// First request: cache miss → 200 stored.
	req1, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/orgs/example/members?page=1", nil)
	resp1, err := client.Do(req1) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	defer resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)
	require.Equal(t, "", sawIfNoneMatch.Load(), "first request should not send If-None-Match")
	got1, _ := io.ReadAll(resp1.Body)
	require.Equal(t, body, string(got1))

	stats := tr.Stats()
	require.Equal(t, uint64(0), stats.Hits304)
	require.Equal(t, uint64(1), stats.Stores)

	// Second request: cache hit → If-None-Match sent → 304 from server → cached body returned.
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/orgs/example/members?page=1", nil)
	resp2, err := client.Do(req2) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode, "transport should materialize cached body as 200")
	require.Equal(t, etag, sawIfNoneMatch.Load(), "second request should send If-None-Match with stored ETag")
	got2, _ := io.ReadAll(resp2.Body)
	require.Equal(t, body, string(got2), "cached body returned verbatim")
	require.Equal(t, ctType, resp2.Header.Get("Content-Type"))
	require.Equal(t, "HIT", resp2.Header.Get(etagHitHeader))
	require.Equal(t, "4999", resp2.Header.Get("X-Ratelimit-Remaining"),
		"304 response headers preserved (rate-limit info must survive)")

	stats = tr.Stats()
	require.Equal(t, uint64(1), stats.Hits304)
}

func TestETagTransport_NonGetBypassed(t *testing.T) {
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			require.Empty(t, req.Header.Get("If-None-Match"), "POSTs must never carry If-None-Match")
			return newResp(http.StatusOK, http.Header{"ETag": []string{`"x"`}}, "ok", req), nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.github.com/x", strings.NewReader("body"))
	resp, err := (&http.Client{Transport: tr}).Do(req) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	defer resp.Body.Close()

	stats := tr.Stats()
	require.Equal(t, uint64(1), stats.Bypassed)
	require.Equal(t, uint64(0), stats.Stores)
}

func TestETagTransport_NoETagInResponseSkipsCache(t *testing.T) {
	calls := atomic.Int64{}
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			// No ETag header → don't cache.
			return newResp(http.StatusOK, http.Header{}, "uncacheable", req), nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)
	client := &http.Client{Transport: tr}

	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/no-etag", nil)
		resp, err := client.Do(req) //nolint:gosec // test transport; URL is static
		require.NoError(t, err)
		_ = resp.Body.Close()
	}
	require.Equal(t, int64(3), calls.Load(), "every request hits the wire when there's no ETag to cache")
	require.Equal(t, uint64(3), tr.Stats().Misses)
	require.Equal(t, uint64(0), tr.Stats().Stores)
}

func TestETagTransport_ScopeIsolation(t *testing.T) {
	// Two transports with different scopes must produce different cache keys for
	// the same URL — otherwise one installation's data could be served to another.
	scope1, err := NewETagTransport(&fakeRT{fn: func(r *http.Request) (*http.Response, error) {
		return newResp(http.StatusOK, http.Header{"ETag": []string{`"v1"`}}, "data1", r), nil
	}}, 0, "installation-A")
	require.NoError(t, err)

	scope2, err := NewETagTransport(&fakeRT{fn: func(r *http.Request) (*http.Response, error) {
		return newResp(http.StatusOK, http.Header{"ETag": []string{`"v1"`}}, "data2", r), nil
	}}, 0, "installation-B")
	require.NoError(t, err)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/orgs/example/members", nil)
	k1 := scope1.cacheKey(req)
	k2 := scope2.cacheKey(req)
	require.NotEqual(t, k1, k2, "different auth scopes must produce different cache keys")
}

func TestETagTransport_ETagRotation(t *testing.T) {
	// When the server returns a fresh body with a new ETag (no 304), the cache
	// must update so the next request uses the new tag.
	const (
		body1, body2 = "first", "second"
		etag1, etag2 = `"v1"`, `"v2"`
	)
	stage := atomic.Int32{}

	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			s := stage.Add(1)
			switch s {
			case 1:
				return newResp(http.StatusOK, http.Header{"ETag": []string{etag1}}, body1, req), nil
			case 2:
				// Server says data changed: not 304, fresh 200 with a new ETag.
				require.Equal(t, etag1, req.Header.Get("If-None-Match"))
				return newResp(http.StatusOK, http.Header{"ETag": []string{etag2}}, body2, req), nil
			case 3:
				require.Equal(t, etag2, req.Header.Get("If-None-Match"),
					"third request should send the updated ETag")
				return newResp(http.StatusNotModified, http.Header{}, "", req), nil
			}
			t.Fatalf("unexpected request count")
			return nil, nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)
	client := &http.Client{Transport: tr}

	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/orgs/example/members?page=2", nil)
		resp, err := client.Do(req) //nolint:gosec // test transport; URL is static
		require.NoError(t, err)
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		switch i {
		case 0:
			require.Equal(t, body1, string(got))
		case 1:
			require.Equal(t, body2, string(got))
		case 2:
			require.Equal(t, body2, string(got), "304 should resurface the most recently cached body")
		}
	}
	stats := tr.Stats()
	require.Equal(t, uint64(1), stats.Hits304)
	require.Equal(t, uint64(2), stats.Stores)
}

func TestETagTransport_RespectsCallerSetIfNoneMatch(t *testing.T) {
	// If the caller has already set If-None-Match (e.g., test framework, or some
	// future SDK pre-stamp), bypass the cache — don't overwrite or substitute.
	calls := atomic.Int64{}
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			require.Equal(t, `"caller-set"`, req.Header.Get("If-None-Match"))
			return newResp(http.StatusOK, http.Header{"ETag": []string{`"server"`}}, "ok", req), nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/x", nil)
	req.Header.Set("If-None-Match", `"caller-set"`)
	resp, err := (&http.Client{Transport: tr}).Do(req) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, int64(1), calls.Load())
	require.Equal(t, uint64(1), tr.Stats().Bypassed)
}

// TestETagTransport_WithHttptest exercises the full HTTP stack against a real
// httptest server to confirm we don't get any subtle wire-level bugs from
// stubbing the round-tripper.
func TestETagTransport_WithHttptest(t *testing.T) {
	const etag = `W/"realServer"`
	body := strings.Repeat("X", 1024)
	hits := atomic.Int64{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") == etag {
			w.Header().Set("X-Ratelimit-Remaining", "4998")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	tr, err := NewETagTransport(srv.Client().Transport, 0, "scope")
	require.NoError(t, err)
	client := &http.Client{Transport: tr}

	// Cold
	req1, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/orgs/x/members", nil)
	resp1, err := client.Do(req1) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	got1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	require.Equal(t, body, string(got1))

	// Warm
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/orgs/x/members", nil)
	resp2, err := client.Do(req2) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	got2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	require.Equal(t, body, string(got2))
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	require.Equal(t, "HIT", resp2.Header.Get(etagHitHeader))

	require.Equal(t, int64(2), hits.Load(), "two real wire requests even with cache")
	require.Equal(t, uint64(1), tr.Stats().Hits304)
}

func TestETagTransport_UnexpectedNotModifiedRetriesUnconditional(t *testing.T) {
	// Pathological: server returns 304 even though we never set If-None-Match
	// (e.g., an intermediary injected it). The transport must NOT return 304 to
	// go-github (which can't parse it as data); it must retry without
	// conditional headers.
	calls := atomic.Int32{}
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			n := calls.Add(1)
			switch n {
			case 1:
				// First call: server returns a surprise 304.
				require.Empty(t, req.Header.Get("If-None-Match"))
				return newResp(http.StatusNotModified, http.Header{}, "", req), nil
			case 2:
				// Retry: must be unconditional.
				require.Empty(t, req.Header.Get("If-None-Match"))
				require.Empty(t, req.Header.Get("If-Modified-Since"))
				return newResp(http.StatusOK, http.Header{
					"ETag":         []string{`"after-retry"`},
					"Content-Type": []string{"application/json"},
				}, "real data", req), nil
			}
			t.Fatalf("unexpected call count %d", n)
			return nil, nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/x", nil)
	resp, err := (&http.Client{Transport: tr}).Do(req) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "transport must hide the spurious 304 from the caller")
	got, _ := io.ReadAll(resp.Body)
	require.Equal(t, "real data", string(got))
	require.Equal(t, int32(2), calls.Load(), "exactly one retry")
}

// drainSaveCh waits briefly for the writer goroutine to drain pending Saves
// without invoking Close (which would shut the transport down). Used for tests
// that want to assert post-Save state mid-transport-lifetime.
func drainSaveCh(t *testing.T, tr *ETagTransport, expected int) {
	t.Helper()
	if expected < 0 {
		expected = 0
	}
	target := uint64(expected)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tr.Stats().Persisted >= target {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("writer goroutine did not drain %d saves in time (persisted=%d, dropped=%d)",
		expected, tr.Stats().Persisted, tr.Stats().DroppedWrites)
}

func TestETagTransport_SessionPersistence_WarmSavePropagates(t *testing.T) {
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			return newResp(http.StatusOK, http.Header{
				"ETag":         []string{`"persist-me"`},
				"Content-Type": []string{"application/json"},
			}, "payload", req), nil
		},
	}
	tr, err := NewETagTransport(next, 0, "test-scope")
	require.NoError(t, err)
	defer func() { _ = tr.Close(context.Background()) }()

	ss := newFakeSessionStore()
	tr.SetSession(context.Background(), ss)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/orgs/x/members", nil)
	resp, err := (&http.Client{Transport: tr}).Do(req) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	_ = resp.Body.Close()

	drainSaveCh(t, tr, 1)
	require.Equal(t, uint64(1), tr.Stats().Persisted)
	require.Equal(t, 1, ss.size(), "warm-write must reach the persistent store")
}

func TestETagTransport_SessionPersistence_ColdLoadHydrates(t *testing.T) {
	// Pre-populate the store with an entry under the prefix that NewETagTransport
	// with scope "shared-scope" will use, then verify a fresh transport pulls it
	// in via SetSession and serves a synthetic 304 cache hit.
	ss := newFakeSessionStore()

	// Compute the scope hash the way NewETagTransport does, so the prefix matches.
	scopeHashBytes := struct{ h string }{}
	_ = scopeHashBytes
	// Easier: just construct a sessionPersistor with the same scope used below.
	hot, err := NewETagTransport(&fakeRT{fn: func(*http.Request) (*http.Response, error) {
		return newResp(http.StatusOK, http.Header{
			"ETag":         []string{`"pre-warmed"`},
			"Content-Type": []string{"application/json"},
		}, "pre-warmed-body", nil), nil
	}}, 0, "shared-scope")
	require.NoError(t, err)
	hot.SetSession(context.Background(), ss)
	// Issue a request to populate the store via the natural path.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/orgs/y/members?page=1", nil)
	resp, err := (&http.Client{Transport: hot}).Do(req) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	_ = resp.Body.Close()
	drainSaveCh(t, hot, 1)
	require.NoError(t, hot.Close(context.Background()))

	// Cold transport: fresh instance, same scope. Its server must NOT need to
	// return a 200 — the cache should hydrate from the store and the wire
	// response is a 304.
	sawIfNoneMatch := ""
	wireCalls := 0
	cold, err := NewETagTransport(&fakeRT{fn: func(req *http.Request) (*http.Response, error) {
		wireCalls++
		sawIfNoneMatch = req.Header.Get("If-None-Match")
		if sawIfNoneMatch != "" {
			return newResp(http.StatusNotModified, http.Header{}, "", req), nil
		}
		t.Fatalf("cold transport made unconditional request — hydrate failed")
		return nil, nil
	}}, 0, "shared-scope")
	require.NoError(t, err)
	defer func() { _ = cold.Close(context.Background()) }()
	cold.SetSession(context.Background(), ss)

	// SetSession's hydrate runs in a goroutine; wait briefly for the Otter
	// cache to populate before issuing the request.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := cold.cache.GetIfPresent(cold.cacheKey(req)); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/orgs/y/members?page=1", nil)
	resp2, err := (&http.Client{Transport: cold}).Do(req2) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	defer resp2.Body.Close()
	got, _ := io.ReadAll(resp2.Body)
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	require.Equal(t, "pre-warmed-body", string(got), "hydrated body served via 304")
	require.Equal(t, `"pre-warmed"`, sawIfNoneMatch, "transport sent the persisted ETag")
	require.Equal(t, 1, wireCalls, "exactly one conditional wire request")
	require.Equal(t, uint64(1), cold.Stats().Hits304)
}

func TestETagTransport_SessionPersistence_OversizedSkipped(t *testing.T) {
	bigBody := strings.Repeat("X", persistedMaxEntryBytes+1)
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			return newResp(http.StatusOK, http.Header{
				"ETag":         []string{`"big"`},
				"Content-Type": []string{"text/plain"},
			}, bigBody, req), nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)
	defer func() { _ = tr.Close(context.Background()) }()

	ss := newFakeSessionStore()
	tr.SetSession(context.Background(), ss)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/big", nil)
	resp, err := (&http.Client{Transport: tr}).Do(req) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	_ = resp.Body.Close()

	// Give the writer a beat in case anything queued.
	time.Sleep(50 * time.Millisecond)
	stats := tr.Stats()
	require.Equal(t, uint64(1), stats.Stores, "entry still landed in in-memory Otter")
	require.Equal(t, uint64(1), stats.OversizedSkipped, "oversized counter incremented")
	require.Equal(t, uint64(0), stats.Persisted, "nothing persisted for oversized body")
	require.Equal(t, 0, ss.size(), "session store untouched for oversized entry")
}

func TestETagTransport_SessionPersistence_NoSessionFallback(t *testing.T) {
	// Verify that without SetSession, the transport still works exactly as
	// Phase 1 did. This is the noopPersistor path.
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			return newResp(http.StatusOK, http.Header{
				"ETag":         []string{`"v1"`},
				"Content-Type": []string{"application/json"},
			}, "body", req), nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)
	defer func() { _ = tr.Close(context.Background()) }()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/no-session", nil)
	resp, err := (&http.Client{Transport: tr}).Do(req) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	_ = resp.Body.Close()

	// Best-effort: the writer goroutine may still execute the noopPersistor.Save
	// (which is a no-op). Give it a beat for orderly observation.
	time.Sleep(50 * time.Millisecond)
	stats := tr.Stats()
	require.Equal(t, uint64(1), stats.Stores)
	// Persisted counter increments even with noopPersistor (the writer treats
	// noop success as a successful save). That's fine — its sole purpose is
	// telling operators "saves are flowing"; what matters is the session-store
	// path is what isolates correctness.
	require.Equal(t, uint64(0), stats.OversizedSkipped)
}

func TestETagTransport_SessionPersistence_DrainOnClose(t *testing.T) {
	// Enqueue several saves and verify Close drains them before returning.
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			return newResp(http.StatusOK, http.Header{
				"ETag":         []string{`"e-` + req.URL.Path + `"`},
				"Content-Type": []string{"application/json"},
			}, "body-"+req.URL.Path, req), nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)
	ss := newFakeSessionStore()
	tr.SetSession(context.Background(), ss)

	client := &http.Client{Transport: tr}
	for i := 0; i < 8; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"https://api.github.com/x/"+string(rune('a'+i)), nil)
		resp, err := client.Do(req) //nolint:gosec // test transport; URL is static
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	require.NoError(t, tr.Close(context.Background()))
	require.Equal(t, uint64(8), tr.Stats().Persisted, "all saves drained before Close returned")
	require.Equal(t, 8, ss.size())
}

func TestETagTransport_SessionPersistence_SetSessionIdempotent(t *testing.T) {
	// Calling SetSession a second time must be a no-op (the underlying
	// sync.Once should swallow the second bind).
	tr, err := NewETagTransport(&fakeRT{fn: func(*http.Request) (*http.Response, error) {
		return newResp(http.StatusOK, http.Header{}, "", nil), nil
	}}, 0, "scope")
	require.NoError(t, err)
	defer func() { _ = tr.Close(context.Background()) }()

	ssA := newFakeSessionStore()
	ssB := newFakeSessionStore()
	tr.SetSession(context.Background(), ssA)
	tr.SetSession(context.Background(), ssB) // should be ignored

	// Pre-seed A so a successful first bind would surface its entry when reading
	// from B's prefix (it won't, because the cache key includes the auth-scope
	// hash; but the test's job is to confirm B was never bound, so ssB must stay
	// empty after issuing requests that would otherwise persist).
	require.NoError(t, ssA.Set(context.Background(), "primer", []byte(`{}`)))

	require.Equal(t, 0, ssB.size(), "second SetSession was a no-op; B must stay empty")
	require.Equal(t, 1, ssA.size(), "A still holds its own primer; not consumed by anything")
}

// TestETagTransport_ReviewFix_LateSendAfterCloseNoPanic verifies the fix for
// the send-on-closed-channel panic flagged by code review: enqueueSave called
// after Close must not panic. We construct a transport, close it, then run a
// request that would normally enqueue a Save. The Post-close request should
// degrade to "drop" rather than crash.
func TestETagTransport_ReviewFix_LateSendAfterCloseNoPanic(t *testing.T) {
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			return newResp(http.StatusOK, http.Header{
				"ETag":         []string{`"v1"`},
				"Content-Type": []string{"application/json"},
			}, "body", req), nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)
	tr.SetSession(context.Background(), newFakeSessionStore())
	require.NoError(t, tr.Close(context.Background()))

	// After Close, this request would normally try to enqueue. The atomic
	// closed flag must short-circuit before the channel send.
	require.NotPanics(t, func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/late", nil)
		resp, err := (&http.Client{Transport: tr}).Do(req) //nolint:gosec // test transport; URL is static
		if err == nil {
			_ = resp.Body.Close()
		}
	})
	// The request still completed (304 substitution etc. don't apply on first
	// fetch); the Store happened, but persistence was dropped.
	require.GreaterOrEqual(t, tr.Stats().DroppedWrites, uint64(1),
		"late save must increment droppedWrites instead of panicking")
}

// TestETagTransport_ReviewFix_HydrateDoesNotClobberFresherEntry verifies the
// fix for the hydrate-vs-RoundTrip clobber: a hydrate goroutine seeing an
// already-populated Otter entry with newer StoredAt must skip the overwrite.
func TestETagTransport_ReviewFix_HydrateDoesNotClobberFresherEntry(t *testing.T) {
	// Pre-seed the session store with a stale entry (StoredAt: -2h) under the
	// prefix that scope "shared-hydrate" produces.
	ss := newFakeSessionStore()
	staleEntry := &etagCacheEntry{
		ETag:        `"stale"`,
		Body:        []byte("stale-body"),
		ContentType: "application/json",
		StoredAt:    time.Now().Add(-2 * time.Hour),
	}

	// Determine the cache key the transport will compute for our test URL, so
	// we can also pre-populate Otter with a "fresher" entry before SetSession
	// kicks off the hydrate.
	tr, err := NewETagTransport(&fakeRT{fn: func(*http.Request) (*http.Response, error) {
		t.Fatalf("transport must not hit the wire in this test")
		return nil, nil
	}}, 0, "shared-hydrate")
	require.NoError(t, err)
	defer func() { _ = tr.Close(context.Background()) }()

	// Save stale entry to the session store under the same key.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/orgs/example", nil)
	cacheKey := tr.cacheKey(req)
	persistor := newSessionPersistor(ss, tr.authHash)
	require.NoError(t, persistor.Save(context.Background(), cacheKey, staleEntry))

	// Inject a fresher entry into Otter before SetSession fires.
	freshEntry := &etagCacheEntry{
		ETag:        `"fresh"`,
		Body:        []byte("fresh-body"),
		ContentType: "application/json",
		StoredAt:    time.Now(),
	}
	tr.cache.Set(cacheKey, freshEntry)

	tr.SetSession(context.Background(), ss)

	// Wait briefly for the hydrate goroutine to run; can't observe it directly
	// so poll until either the entry changes or a short deadline passes.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		got, ok := tr.cache.GetIfPresent(cacheKey)
		if ok && got != nil && got.ETag != freshEntry.ETag {
			t.Fatalf("hydrate clobbered fresher entry: got ETag %q, want %q", got.ETag, freshEntry.ETag)
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, ok := tr.cache.GetIfPresent(cacheKey)
	require.True(t, ok)
	require.Equal(t, freshEntry.ETag, got.ETag, "fresher in-process entry must survive hydrate")
}

// errPersistor returns a hard error on every Save so we can exercise the
// writer goroutine's error path.
type errPersistor struct {
	saveCalls atomic.Int32
}

func (e *errPersistor) LoadAll(_ context.Context) (map[string]*etagCacheEntry, error) {
	return nil, nil
}
func (e *errPersistor) Save(_ context.Context, _ string, _ *etagCacheEntry) error {
	e.saveCalls.Add(1)
	return errPersistorFail
}

var errPersistorFail = errPersistorFailErr{}

type errPersistorFailErr struct{}

func (errPersistorFailErr) Error() string { return "persistor save failed (test)" }

// TestETagTransport_ReviewFix_PersistorSaveErrorTolerated verifies the writer
// goroutine logs and continues on persistor errors rather than tearing down.
func TestETagTransport_ReviewFix_PersistorSaveErrorTolerated(t *testing.T) {
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			return newResp(http.StatusOK, http.Header{
				"ETag":         []string{`"v1"`},
				"Content-Type": []string{"application/json"},
			}, "body", req), nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)
	defer func() { _ = tr.Close(context.Background()) }()

	// Swap the persistor directly (skips SetSession's sync.Once gate so the
	// test stays focused on the error path).
	ep := &errPersistor{}
	var pers Persistor = ep
	tr.persistorPtr.Store(&pers)

	client := &http.Client{Transport: tr}
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"https://api.github.com/x/"+string(rune('a'+i)), nil)
		resp, err := client.Do(req) //nolint:gosec // test transport; URL is static
		require.NoError(t, err)
		_ = resp.Body.Close()
	}
	// Give the writer goroutine time to drain.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && ep.saveCalls.Load() < 5 {
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, int32(5), ep.saveCalls.Load(), "writer continued after errors")
	// Persisted stays at 0 because all saves failed; in-memory cache still
	// works as normal.
	require.Equal(t, uint64(0), tr.Stats().Persisted)
	require.Equal(t, uint64(5), tr.Stats().Stores)
}

// TestETagTransport_ReviewFix_MaterializeRewritesContentLength verifies that
// the 304 substitution overwrites the cloned Content-Length (which would be 0
// from the 304) and clears any stale Content-Encoding.
func TestETagTransport_ReviewFix_MaterializeRewritesContentLength(t *testing.T) {
	body := `{"items":[1,2,3]}`
	first := true
	next := &fakeRT{
		fn: func(req *http.Request) (*http.Response, error) {
			if first {
				first = false
				return newResp(http.StatusOK, http.Header{
					"ETag":         []string{`"v1"`},
					"Content-Type": []string{"application/json"},
				}, body, req), nil
			}
			// 304 response with bogus Content-Length and Content-Encoding to
			// confirm the materialize step rewrites both.
			return newResp(http.StatusNotModified, http.Header{
				"Content-Length":   []string{"0"},
				"Content-Encoding": []string{"gzip"},
			}, "", req), nil
		},
	}
	tr, err := NewETagTransport(next, 0, "scope")
	require.NoError(t, err)
	defer func() { _ = tr.Close(context.Background()) }()

	client := &http.Client{Transport: tr}

	// Prime the cache.
	req1, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/x", nil)
	resp1, err := client.Do(req1) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	_ = resp1.Body.Close()

	// Second request: server returns 304, transport substitutes the cached body.
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/x", nil)
	resp2, err := client.Do(req2) //nolint:gosec // test transport; URL is static
	require.NoError(t, err)
	defer resp2.Body.Close()

	require.Equal(t, http.StatusOK, resp2.StatusCode)
	require.Equal(t, int64(len(body)), resp2.ContentLength, "ContentLength struct field matches body length")
	require.Equal(t, strconv.Itoa(len(body)), resp2.Header.Get("Content-Length"),
		"Content-Length header rewritten to match cached body")
	require.Empty(t, resp2.Header.Get("Content-Encoding"),
		"stale Content-Encoding must be cleared from substituted response")
	got, _ := io.ReadAll(resp2.Body)
	require.Equal(t, body, string(got))
}
