package connector

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

type stubRefresher struct {
	mu     sync.Mutex
	calls  int
	tokens []string
}

func (s *stubRefresher) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.tokens) == 0 {
		return nil, errors.New("no more tokens")
	}
	t := s.tokens[0]
	s.tokens = s.tokens[1:]
	return &oauth2.Token{AccessToken: t, Expiry: time.Now().Add(time.Hour)}, nil
}

func TestRefreshableTokenSource_ReusesCachedToken(t *testing.T) {
	refresher := &stubRefresher{tokens: []string{"never-called"}}
	src := newRefreshableTokenSource(
		&oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(time.Hour)},
		refresher,
	)

	for range 3 {
		tok, err := src.Token()
		require.NoError(t, err)
		require.Equal(t, "initial", tok.AccessToken)
	}
	require.Equal(t, 0, refresher.calls)
}

func TestRefreshableTokenSource_RefreshesWhenExpired(t *testing.T) {
	refresher := &stubRefresher{tokens: []string{"refreshed"}}
	src := newRefreshableTokenSource(
		&oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(-time.Minute)},
		refresher,
	)
	tok, err := src.Token()
	require.NoError(t, err)
	require.Equal(t, "refreshed", tok.AccessToken)
	require.Equal(t, 1, refresher.calls)
}

func TestRefreshableTokenSource_InvalidateForcesRefresh(t *testing.T) {
	refresher := &stubRefresher{tokens: []string{"refreshed"}}
	initial := &oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(time.Hour)}
	src := newRefreshableTokenSource(initial, refresher)

	src.Invalidate(initial)

	tok, err := src.Token()
	require.NoError(t, err)
	require.Equal(t, "refreshed", tok.AccessToken)
	require.Equal(t, 1, refresher.calls)
}

func TestRefreshableTokenSource_InvalidateIgnoresStaleToken(t *testing.T) {
	refresher := &stubRefresher{tokens: []string{"second"}}
	initial := &oauth2.Token{AccessToken: "first", Expiry: time.Now().Add(time.Hour)}
	src := newRefreshableTokenSource(initial, refresher)

	// Simulate the cache already advancing to "second" via expiry refresh.
	src.cur = &oauth2.Token{AccessToken: "second", Expiry: time.Now().Add(time.Hour)}

	// A stale caller still holds "first" and tries to invalidate; we must not
	// clear the newer cached token.
	src.Invalidate(initial)

	tok, err := src.Token()
	require.NoError(t, err)
	require.Equal(t, "second", tok.AccessToken)
	require.Equal(t, 0, refresher.calls)
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func makeResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestUnauthorizedRefreshTransport_RetriesOn401(t *testing.T) {
	refresher := &stubRefresher{tokens: []string{"refreshed"}}
	initial := &oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(time.Hour)}
	src := newRefreshableTokenSource(initial, refresher)

	var calls int32
	var seenAuth []string
	// Simulate oauth2.Transport: fetch a token on every request and 401 if the
	// token isn't the refreshed one.
	base := rtFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		tok, err := src.Token()
		if err != nil {
			return nil, err
		}
		seenAuth = append(seenAuth, tok.AccessToken)
		if tok.AccessToken != "refreshed" {
			return makeResp(http.StatusUnauthorized, "nope"), nil
		}
		return makeResp(http.StatusOK, "ok"), nil
	})
	tr := &unauthorizedRefreshTransport{base: base, src: src}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/x", nil)
	require.NoError(t, err)
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))
	require.Equal(t, []string{"initial", "refreshed"}, seenAuth)
	require.Equal(t, 1, refresher.calls, "cache should have been invalidated and refreshed once")
}

func TestUnauthorizedRefreshTransport_PassesThroughNon401(t *testing.T) {
	refresher := &stubRefresher{tokens: []string{"unused"}}
	initial := &oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(time.Hour)}
	src := newRefreshableTokenSource(initial, refresher)

	var calls int32
	base := rtFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return makeResp(http.StatusForbidden, "denied"), nil
	})
	tr := &unauthorizedRefreshTransport{base: base, src: src}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/x", nil)
	require.NoError(t, err)
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))
	require.Equal(t, 0, refresher.calls)
}

func TestUnauthorizedRefreshTransport_GetBodyErrorReturnsReadableResponse(t *testing.T) {
	refresher := &stubRefresher{tokens: []string{"unused"}}
	initial := &oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(time.Hour)}
	src := newRefreshableTokenSource(initial, refresher)

	var calls int32
	base := rtFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return makeResp(http.StatusUnauthorized, "details about the 401"), nil
	})
	tr := &unauthorizedRefreshTransport{base: base, src: src}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.github.com/x", strings.NewReader("payload"))
	require.NoError(t, err)
	req.GetBody = func() (io.ReadCloser, error) {
		return nil, errors.New("synthetic GetBody failure")
	}

	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "must not retry when body cannot be rebuilt")

	// Body must still be readable by the caller; the prior bug closed it.
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "details about the 401", string(body))
	require.Equal(t, 0, refresher.calls, "token must not be invalidated when we couldn't retry")
}

func TestUnauthorizedRefreshTransport_DoesNotRetryWhenBodyCannotReplay(t *testing.T) {
	refresher := &stubRefresher{tokens: []string{"unused"}}
	initial := &oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(time.Hour)}
	src := newRefreshableTokenSource(initial, refresher)

	var calls int32
	base := rtFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return makeResp(http.StatusUnauthorized, "nope"), nil
	})
	tr := &unauthorizedRefreshTransport{base: base, src: src}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.github.com/x", strings.NewReader("payload"))
	require.NoError(t, err)
	req.GetBody = nil // simulate unbufferable body
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))
}
