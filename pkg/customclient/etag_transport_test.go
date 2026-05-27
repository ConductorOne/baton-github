package customclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
