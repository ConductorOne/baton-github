package connector

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

type countingTokenSource struct {
	calls atomic.Int64
}

func (s *countingTokenSource) Token() (*oauth2.Token, error) {
	n := s.calls.Add(1)
	return &oauth2.Token{
		AccessToken: "fresh-token",
		Expiry:      time.Now().Add(time.Hour),
		TokenType:   "Bearer",
		ExpiresIn:   int64(n),
	}, nil
}

func TestInvalidatableTokenSource_CachesToken(t *testing.T) {
	src := &countingTokenSource{}
	ts := newInvalidatableTokenSource(nil, src)

	tok1, err := ts.Token()
	require.NoError(t, err)
	require.Equal(t, "fresh-token", tok1.AccessToken)

	tok2, err := ts.Token()
	require.NoError(t, err)
	require.Equal(t, tok1, tok2)
	require.Equal(t, int64(1), src.calls.Load())
}

func TestInvalidatableTokenSource_RefreshesAfterInvalidate(t *testing.T) {
	src := &countingTokenSource{}
	ts := newInvalidatableTokenSource(nil, src)

	_, err := ts.Token()
	require.NoError(t, err)
	require.Equal(t, int64(1), src.calls.Load())

	ts.Invalidate()

	_, err = ts.Token()
	require.NoError(t, err)
	require.Equal(t, int64(2), src.calls.Load())
}

func TestInvalidatableTokenSource_EarlyExpiry(t *testing.T) {
	src := &countingTokenSource{}
	initial := &oauth2.Token{
		AccessToken: "initial",
		Expiry:      time.Now().Add(tokenEarlyExpiry - time.Second),
	}
	ts := newInvalidatableTokenSource(initial, src)

	tok, err := ts.Token()
	require.NoError(t, err)
	require.Equal(t, "fresh-token", tok.AccessToken)
	require.Equal(t, int64(1), src.calls.Load())
}

func TestRetryOn401Transport(t *testing.T) {
	var reqCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "Bearer fresh-token" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"still bad"}`))
	}))
	defer server.Close()

	src := &countingTokenSource{}
	ts := newInvalidatableTokenSource(nil, src)

	transport := &retryOn401Transport{
		base: &oauth2.Transport{
			Base:   http.DefaultTransport,
			Source: ts,
		},
		ts: ts,
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), "ok")
	require.Equal(t, int64(2), reqCount.Load())
	require.Equal(t, int64(2), src.calls.Load())
}

func TestRetryOn401Transport_NoRetryOnSuccess(t *testing.T) {
	var reqCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	src := &countingTokenSource{}
	ts := newInvalidatableTokenSource(nil, src)

	transport := &retryOn401Transport{
		base: &oauth2.Transport{
			Base:   http.DefaultTransport,
			Source: ts,
		},
		ts: ts,
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int64(1), reqCount.Load())
}

func TestRetryOn401Transport_PermanentAuthFailure(t *testing.T) {
	var reqCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer server.Close()

	src := &countingTokenSource{}
	ts := newInvalidatableTokenSource(nil, src)

	transport := &retryOn401Transport{
		base: &oauth2.Transport{
			Base:   http.DefaultTransport,
			Source: ts,
		},
		ts: ts,
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, int64(2), reqCount.Load())
}
