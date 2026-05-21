package connector

import (
	"fmt"
	"io"
	"net/http"

	"github.com/conductorone/baton-sdk/pkg/ratelimit"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"google.golang.org/grpc/status"
)

// graphqlStatusClassifyingTransport converts HTTP 429 and 5xx responses from
// the configured GraphQL host into gRPC-classified errors using the same
// status-to-code mapping the SDK applies to its own HTTP responses
// (uhttp.GrpcCodeFromHTTPStatus). Without this, shurcooL/graphql surfaces
// non-200 responses as opaque fmt.Errorf strings, which propagate as
// codes.Unknown and abort the sync on a single transient blip.
type graphqlStatusClassifyingTransport struct {
	base        http.RoundTripper
	graphqlHost string
}

func (t *graphqlStatusClassifyingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if req.URL.Host != t.graphqlHost {
		return resp, nil
	}
	if !shouldClassifyGraphQLStatus(resp.StatusCode) {
		return resp, nil
	}
	rlDesc, _ := ratelimit.ExtractRateLimitData(resp.StatusCode, &resp.Header)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	msg := fmt.Sprintf("%s %s: HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
	if s := resp.Header.Get("Server"); s != "" {
		msg += " server=" + s
	}
	if rid := resp.Header.Get("X-GitHub-Request-Id"); rid != "" {
		msg += " request-id=" + rid
	}
	if len(body) > 0 {
		msg += ": " + string(body)
	}
	st := status.New(uhttp.GrpcCodeFromHTTPStatus(resp.StatusCode), msg)
	if rlDesc != nil {
		if withDetails, err := st.WithDetails(rlDesc); err == nil {
			st = withDetails
		}
	}
	return nil, st.Err()
}

func shouldClassifyGraphQLStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}
