package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGraphQLStatusClassifyingTransport_ClassifiesUnavailable(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
	}{
		{"429", http.StatusTooManyRequests},
		{"500", http.StatusInternalServerError},
		{"502", http.StatusBadGateway},
		{"503", http.StatusServiceUnavailable},
		{"504", http.StatusGatewayTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-GitHub-Request-Id", "req-abc")
				w.Header().Set("Server", "test-server")
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte("upstream connect error"))
			}))
			t.Cleanup(srv.Close)

			u, err := url.Parse(srv.URL)
			require.NoError(t, err)

			client := &http.Client{
				Transport: &graphqlStatusClassifyingTransport{
					base:        http.DefaultTransport,
					graphqlHost: u.Host,
				},
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/graphql", nil)
			require.NoError(t, err)
			resp, err := client.Do(req) //nolint:bodyclose // transport drains and returns nil resp on classification
			require.Nil(t, resp)
			require.Error(t, err)
			require.Equal(t, codes.Unavailable, status.Code(err))
			require.Contains(t, err.Error(), "request-id=req-abc")
			require.Contains(t, err.Error(), "server=test-server")
			require.Contains(t, err.Error(), "upstream connect error")
		})
	}
}

func TestGraphQLStatusClassifyingTransport_PassesThrough2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	client := &http.Client{
		Transport: &graphqlStatusClassifyingTransport{
			base:        http.DefaultTransport,
			graphqlHost: u.Host,
		},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestGraphQLStatusClassifyingTransport_PassesThroughNon429Client4xx(t *testing.T) {
	cases := []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
	}
	for _, code := range cases {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			t.Cleanup(srv.Close)

			u, err := url.Parse(srv.URL)
			require.NoError(t, err)

			client := &http.Client{
				Transport: &graphqlStatusClassifyingTransport{
					base:        http.DefaultTransport,
					graphqlHost: u.Host,
				},
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
			require.NoError(t, err)
			resp, err := client.Do(req)
			require.NoError(t, err)
			require.NotNil(t, resp)
			t.Cleanup(func() { _ = resp.Body.Close() })
			require.Equal(t, code, resp.StatusCode)
		})
	}
}

func TestGraphQLStatusClassifyingTransport_PassesThroughNonMatchingHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable"))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &graphqlStatusClassifyingTransport{
			base:        http.DefaultTransport,
			graphqlHost: "some-other-host.invalid",
		},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
