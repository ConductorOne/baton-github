package connector

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// stubTokenSource returns the next scripted token / error per call.
type stubTokenSource struct {
	mu     sync.Mutex
	calls  int
	tokens []*oauth2.Token
	errs   []error
}

func (s *stubTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.tokens) {
		return nil, errors.New("stubTokenSource exhausted")
	}
	tok, err := s.tokens[s.calls], s.errs[s.calls]
	s.calls++
	return tok, err
}

func (s *stubTokenSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func validToken(access string) *oauth2.Token {
	return &oauth2.Token{AccessToken: access, Expiry: time.Now().Add(time.Hour)}
}

func TestRefreshableTokenSource_UsesInitialUntilInvalidate(t *testing.T) {
	src := &stubTokenSource{tokens: []*oauth2.Token{validToken("refreshed")}, errs: []error{nil}}
	rts := newRefreshableTokenSource(validToken("initial"), src)

	for i := 0; i < 5; i++ {
		tok, err := rts.Token()
		require.NoError(t, err)
		require.Equal(t, "initial", tok.AccessToken)
	}
	require.Equal(t, 0, src.callCount(), "must not refresh while initial token is valid")

	rts.Invalidate()
	tok, err := rts.Token()
	require.NoError(t, err)
	require.Equal(t, "refreshed", tok.AccessToken)
	require.Equal(t, 1, src.callCount())

	for i := 0; i < 3; i++ {
		tok, err := rts.Token()
		require.NoError(t, err)
		require.Equal(t, "refreshed", tok.AccessToken)
	}
	require.Equal(t, 1, src.callCount(), "must not re-fetch while refreshed token is valid")
}

func TestRefreshableTokenSource_NilInitialFetchesOnFirstCall(t *testing.T) {
	src := &stubTokenSource{tokens: []*oauth2.Token{validToken("first")}, errs: []error{nil}}
	rts := newRefreshableTokenSource(nil, src)

	tok, err := rts.Token()
	require.NoError(t, err)
	require.Equal(t, "first", tok.AccessToken)
	require.Equal(t, 1, src.callCount())
}

func TestRefreshableTokenSource_SurfacesSourceError(t *testing.T) {
	src := &stubTokenSource{tokens: []*oauth2.Token{nil}, errs: []error{errors.New("github: bad creds")}}
	rts := newRefreshableTokenSource(nil, src)

	_, err := rts.Token()
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad creds")
	require.Equal(t, 1, src.callCount())
}

// scriptedRoundTripper returns scripted (resp, err) per call.
type scriptedRoundTripper struct {
	mu    sync.Mutex
	t     *testing.T
	calls int
	steps []rtStep
}

type rtStep struct {
	statusCode int
	body       string
	err        error
}

func (r *scriptedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Less(r.t, r.calls, len(r.steps), "unexpected extra RoundTrip call (#%d)", r.calls+1)
	step := r.steps[r.calls]
	r.calls++
	if step.err != nil {
		return nil, step.err
	}
	return &http.Response{
		StatusCode: step.statusCode,
		Body:       io.NopCloser(strings.NewReader(step.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func (r *scriptedRoundTripper) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// tokenFetchingRT simulates oauth2.Transport's behavior: it consults the
// TokenSource on every RoundTrip, which is the moment Invalidate's effect
// becomes observable.
type tokenFetchingRT struct {
	rts  oauth2.TokenSource
	next http.RoundTripper
}

func (t *tokenFetchingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	_, err := t.rts.Token()
	if err != nil {
		return nil, err
	}
	return t.next.RoundTrip(req)
}

func newTransportFixture(t *testing.T, steps ...rtStep) (*tokenRefreshTransport, *scriptedRoundTripper, *refreshableTokenSource, *stubTokenSource) {
	t.Helper()
	src := &stubTokenSource{
		tokens: []*oauth2.Token{validToken("post-refresh")},
		errs:   []error{nil},
	}
	rts := newRefreshableTokenSource(validToken("initial"), src)
	rt := &scriptedRoundTripper{t: t, steps: steps}
	return &tokenRefreshTransport{
		next: &tokenFetchingRT{rts: rts, next: rt},
		rts:  rts,
	}, rt, rts, src
}

func newGetRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/repos/x/y/collaborators", nil)
	require.NoError(t, err)
	return req
}

// closeBody closes resp.Body if resp is non-nil. Used by tests that exercise
// the transport directly and need to satisfy bodyclose.
func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp != nil {
		_ = resp.Body.Close()
	}
}

func TestTokenRefreshTransport_PassesThrough200(t *testing.T) {
	transport, rt, _, src := newTransportFixture(t, rtStep{statusCode: http.StatusOK, body: `{}`})

	resp, err := transport.RoundTrip(newGetRequest(t))
	defer closeBody(t, resp)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 1, rt.callCount())
	require.Equal(t, 0, src.callCount(), "must not Invalidate or re-fetch on 2xx")
}

func TestTokenRefreshTransport_RetriesOn401ThenSucceeds(t *testing.T) {
	transport, rt, rts, src := newTransportFixture(t,
		rtStep{statusCode: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`},
		rtStep{statusCode: http.StatusOK, body: `[]`},
	)

	resp, err := transport.RoundTrip(newGetRequest(t))
	defer closeBody(t, resp)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 2, rt.callCount(), "exactly one retry after 401")
	require.Equal(t, 1, src.callCount(), "must fetch a fresh token from source")

	tok, _ := rts.Token()
	require.Equal(t, "post-refresh", tok.AccessToken, "cache holds refreshed token")
}

func TestTokenRefreshTransport_PersistentUnauthRetriesOnceThenSurfaces(t *testing.T) {
	transport, rt, _, src := newTransportFixture(t,
		rtStep{statusCode: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`},
		rtStep{statusCode: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`},
	)

	resp, err := transport.RoundTrip(newGetRequest(t))
	defer closeBody(t, resp)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, 2, rt.callCount(), "exactly one retry — no infinite loop")
	require.Equal(t, 1, src.callCount(), "Invalidate fires once even on persistent 401")
}

func TestTokenRefreshTransport_TransportErrorPassesThrough(t *testing.T) {
	wantErr := errors.New("dial tcp: connection refused")
	transport, rt, _, src := newTransportFixture(t, rtStep{err: wantErr})

	resp, err := transport.RoundTrip(newGetRequest(t))
	defer closeBody(t, resp)
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 1, rt.callCount(), "transport-level errors are not retried")
	require.Equal(t, 0, src.callCount())
}

func TestTokenRefreshTransport_NoRetryWithBodyAndNoGetBody(t *testing.T) {
	transport, rt, _, src := newTransportFixture(t,
		rtStep{statusCode: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`},
	)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.github.com/orgs/x/invitations", bytes.NewReader([]byte(`{"email":"a@b.c"}`)))
	require.NoError(t, err)
	req.GetBody = nil // explicitly: no rewind capability

	resp, err := transport.RoundTrip(req)
	defer closeBody(t, resp)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "non-rewindable 401 must surface untouched")
	require.Equal(t, 1, rt.callCount(), "non-rewindable bodies must not be retried")
	require.Equal(t, 0, src.callCount())
}

func TestTokenRefreshTransport_NoRetryWhenGetBodyFails(t *testing.T) {
	transport, rt, _, src := newTransportFixture(t,
		rtStep{statusCode: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`},
	)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.github.com/orgs/x/invitations", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.GetBody = func() (io.ReadCloser, error) {
		return nil, errors.New("body already consumed")
	}

	resp, err := transport.RoundTrip(req)
	defer closeBody(t, resp)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, 1, rt.callCount(), "GetBody failure must not trigger a retry")
	require.Equal(t, 0, src.callCount())
}

// End-to-end via the real github.Client: 401 then 200 must produce one
// observable Invalidate (next request fetches a fresh token) and exactly two
// RoundTrip calls — confirming the layered transport stack composes correctly.
func TestTokenRefreshTransport_EndToEndViaGitHubClient(t *testing.T) {
	src := &stubTokenSource{
		tokens: []*oauth2.Token{validToken("post-refresh")},
		errs:   []error{nil},
	}
	rts := newRefreshableTokenSource(validToken("initial"), src)

	var (
		callCount   atomic.Int32
		seenTokens  sync.Map
		serverMu    sync.Mutex
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverMu.Lock()
		defer serverMu.Unlock()
		n := callCount.Add(1)
		auth := r.Header.Get("Authorization")
		seenTokens.Store(int(n), auth)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	httpClient := &http.Client{
		Transport: &tokenRefreshTransport{
			next: &oauth2.Transport{Source: rts}, // no Base — uses http.DefaultTransport
			rts:  rts,
		},
	}
	gc := github.NewClient(httpClient)
	parsed, err := gc.WithEnterpriseURLs(server.URL, server.URL)
	require.NoError(t, err)

	_, _, err = parsed.Repositories.ListCollaborators(context.Background(), "x", "y", &github.ListCollaboratorsOptions{})
	require.NoError(t, err)
	require.Equal(t, int32(2), callCount.Load(), "server saw exactly 2 requests")
	require.Equal(t, 1, src.callCount(), "rts source refreshed once")

	first, _ := seenTokens.Load(1)
	second, _ := seenTokens.Load(2)
	require.Equal(t, "Bearer initial", first, "first attempt used initial token")
	require.Equal(t, "Bearer post-refresh", second, "retry used the refreshed token")
}
