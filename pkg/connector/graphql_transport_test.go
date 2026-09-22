package connector

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestStatusClassifyingTransport_ClassifiesUnavailable(t *testing.T) {
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

			client := &http.Client{
				Transport: &statusClassifyingTransport{base: http.DefaultTransport},
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

func TestStatusClassifyingTransport_PassesThrough2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &statusClassifyingTransport{base: http.DefaultTransport},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestGraphQLErrorsCode(t *testing.T) {
	cases := []struct {
		name  string
		types []string
		want  codes.Code
	}{
		{name: "unclassified", types: []string{"SOMETHING_NEW"}, want: codes.Internal},
		{name: "forbidden", types: []string{"FORBIDDEN"}, want: codes.PermissionDenied},
		{name: "not found", types: []string{"NOT_FOUND"}, want: codes.NotFound},
		{name: "unauthenticated", types: []string{"UNAUTHENTICATED"}, want: codes.Unauthenticated},
		{name: "unprocessable", types: []string{"UNPROCESSABLE"}, want: codes.FailedPrecondition},
		// A rate limit outranks everything: the SDK has to retry rather than
		// fail the sync.
		{name: "rate limit wins", types: []string{"FORBIDDEN", "RATE_LIMITED"}, want: codes.Unavailable},
		// Past a rate limit the first classified entry wins, so a later error
		// cannot mask it. UNPROCESSABLE is what the invite-to-promote fallback
		// in Grant branches on, and a trailing FORBIDDEN used to overwrite it.
		{name: "first classified wins", types: []string{"UNPROCESSABLE", "FORBIDDEN"}, want: codes.FailedPrecondition},
		{name: "order independent", types: []string{"FORBIDDEN", "UNPROCESSABLE"}, want: codes.PermissionDenied},
		// NOT_FOUND is the one code a credential error may override. Callers
		// read it as "already gone" and report success, so a batch whose
		// missing invitations hide a FORBIDDEN must not look benign.
		{name: "credential error beats not found", types: []string{"NOT_FOUND", "FORBIDDEN"}, want: codes.PermissionDenied},
		{name: "credential error beats not found, unauthenticated", types: []string{"NOT_FOUND", "UNAUTHENTICATED"}, want: codes.Unauthenticated},
		{name: "not found alone still maps to not found", types: []string{"NOT_FOUND", "NOT_FOUND"}, want: codes.NotFound},
		// Order must not decide whether Grant's invite-to-promote fallback
		// fires, so UNPROCESSABLE outranks NOT_FOUND from either position.
		{name: "unprocessable beats not found", types: []string{"NOT_FOUND", "UNPROCESSABLE"}, want: codes.FailedPrecondition},
		{name: "unprocessable beats not found, reversed", types: []string{"UNPROCESSABLE", "NOT_FOUND"}, want: codes.FailedPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graphQLErrors := make([]graphQLError, 0, len(tc.types))
			for _, errorType := range tc.types {
				graphQLErrors = append(graphQLErrors, graphQLError{Type: errorType})
			}
			require.Equal(t, tc.want, graphQLErrorsCode(graphQLErrors))
		})
	}
}

// extensions.code is preferred over the top-level type, because GitHub sets it
// on the errors that carry a machine-readable classification.
func TestGraphQLErrorsCodePrefersExtensionsCode(t *testing.T) {
	graphQLErr := graphQLError{Type: "FORBIDDEN"}
	graphQLErr.Extensions.Code = "RATE_LIMITED"

	require.Equal(t, codes.Unavailable, graphQLErrorsCode([]graphQLError{graphQLErr}))
}

func TestEnterpriseGraphQLTransport_PassesThroughUnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &enterpriseGraphQLTransport{base: http.DefaultTransport},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/graphql", nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "not json", string(body))
}

func TestStatusClassifyingTransport_ClassifiesClient4xx(t *testing.T) {
	cases := []struct {
		httpStatus int
		grpcCode   codes.Code
	}{
		{http.StatusBadRequest, codes.InvalidArgument},
		{http.StatusUnauthorized, codes.Unauthenticated},
		{http.StatusForbidden, codes.PermissionDenied},
		{http.StatusNotFound, codes.NotFound},
		{http.StatusConflict, codes.AlreadyExists},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.httpStatus), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.httpStatus)
			}))
			t.Cleanup(srv.Close)

			client := &http.Client{
				Transport: &statusClassifyingTransport{base: http.DefaultTransport},
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/graphql", nil)
			require.NoError(t, err)
			resp, err := client.Do(req) //nolint:bodyclose // transport drains and returns nil resp on classification
			require.Nil(t, resp)
			require.Error(t, err)
			require.Equal(t, tc.grpcCode, status.Code(err))
		})
	}
}

func TestStatusClassifyingTransport_Attaches429RateLimitDetails(t *testing.T) {
	resetAt := time.Now().Add(45 * time.Second).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &statusClassifyingTransport{base: http.DefaultTransport},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/graphql", nil)
	require.NoError(t, err)
	resp, err := client.Do(req) //nolint:bodyclose // transport drains and returns nil resp on classification
	require.Nil(t, resp)
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))

	st, ok := status.FromError(err)
	require.True(t, ok)
	var found *v2.RateLimitDescription
	for _, d := range st.Details() {
		if rl, ok := d.(*v2.RateLimitDescription); ok {
			found = rl
			break
		}
	}
	require.NotNil(t, found, "expected RateLimitDescription detail to be attached for 429")
	require.Equal(t, v2.RateLimitDescription_STATUS_OVERLIMIT, found.GetStatus())
	require.Equal(t, resetAt.Unix(), found.GetResetAt().AsTime().Unix())
}

// TestStatusClassifyingTransport_UsesSDKStatusMapping locks in alignment
// with uhttp.GrpcCodeFromHTTPStatus. 501 is the canary: the SDK maps
// Not Implemented to codes.Unimplemented, so a regression to a hardcoded
// codes.Unavailable for 5xx would flip this assertion.
func TestStatusClassifyingTransport_UsesSDKStatusMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &statusClassifyingTransport{base: http.DefaultTransport},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/graphql", nil)
	require.NoError(t, err)
	resp, err := client.Do(req) //nolint:bodyclose // transport drains and returns nil resp on classification
	require.Nil(t, resp)
	require.Error(t, err)
	require.Equal(t, codes.Unimplemented, status.Code(err))
}
