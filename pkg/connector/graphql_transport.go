package connector

import (
	"fmt"
	"io"
	"net/http"

	"github.com/conductorone/baton-sdk/pkg/ratelimit"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"google.golang.org/grpc/status"
)

// statusClassifyingTransport converts non-2xx HTTP responses into
// gRPC-classified errors using the SDK's canonical status-to-code mapping
// (uhttp.GrpcCodeFromHTTPStatus). This matches how uhttp.BaseHttpClient.Do
// classifies its own responses.
//
// It exists because shurcooL/graphql surfaces non-200 responses as opaque
// fmt.Errorf strings, which otherwise propagate as codes.Unknown and abort
// the sync on a single transient blip — even when the underlying status
// (429, 5xx, 401, 403, 404, ...) carries enough information for the SDK
// retry layer to do the right thing. The REST path doesn't need this because
// go-github exposes structured response/error types that wrapGitHubError
// already classifies at the call site.
type statusClassifyingTransport struct {
	base http.RoundTripper
}

func (t *statusClassifyingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
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
