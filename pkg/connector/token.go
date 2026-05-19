package connector

import (
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// tokenEarlyExpiry is how far before the actual expiry we proactively refresh
// the installation token. This reduces the window where a locally-valid token
// is already expired server-side due to clock drift.
const tokenEarlyExpiry = 5 * time.Minute

// invalidatableTokenSource caches a token from an underlying source and allows
// the cache to be cleared (e.g., after receiving a 401 from the server).
// Unlike oauth2.ReuseTokenSource it applies a configurable early-expiry margin
// and exposes Invalidate() to force a refresh on the next Token() call.
type invalidatableTokenSource struct {
	mu      sync.Mutex
	current *oauth2.Token
	src     oauth2.TokenSource
}

func newInvalidatableTokenSource(initial *oauth2.Token, src oauth2.TokenSource) *invalidatableTokenSource {
	return &invalidatableTokenSource{
		current: initial,
		src:     src,
	}
}

func (s *invalidatableTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current != nil && !s.current.Expiry.IsZero() && time.Now().Before(s.current.Expiry.Add(-tokenEarlyExpiry)) {
		return s.current, nil
	}

	t, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	s.current = t
	return t, nil
}

// Invalidate clears the cached token so the next Token() call will fetch a
// fresh one from the underlying source.
func (s *invalidatableTokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = nil
}

// retryOn401Transport wraps an http.RoundTripper and retries a request once
// when the server returns 401 Unauthorized. Before retrying it invalidates
// the token cache so that the oauth2 transport fetches a fresh token.
type retryOn401Transport struct {
	base http.RoundTripper
	ts   *invalidatableTokenSource
}

func (t *retryOn401Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	// Drain and close the 401 response body before retrying.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	t.ts.Invalidate()

	// Clone the request and remove the stale Authorization header so the
	// oauth2 transport sets a fresh one on retry.
	retryReq := req.Clone(req.Context())
	retryReq.Header.Del("Authorization")

	return t.base.RoundTrip(retryReq)
}
