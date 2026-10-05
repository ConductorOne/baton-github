package connector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/conductorone/baton-sdk/pkg/ratelimit"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"google.golang.org/grpc/codes"
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

// graphQLEnvelope is a GraphQL response body. A partial result populates both
// Data and Errors.
type graphQLEnvelope struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []graphQLError             `json:"errors"`
}

type graphQLError struct {
	Message    string `json:"message"`
	Type       string `json:"type"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// GraphQL error types GitHub returns.
const (
	graphQLErrorNotFound        = "NOT_FOUND"
	graphQLErrorForbidden       = "FORBIDDEN"
	graphQLErrorUnauthenticated = "UNAUTHENTICATED"
	graphQLErrorUnprocessable   = "UNPROCESSABLE"
)

// graphQLErrorType reads the type from either the top-level field or extensions.
func graphQLErrorType(graphQLErr graphQLError) string {
	if code := strings.ToUpper(graphQLErr.Extensions.Code); code != "" {
		return code
	}

	return strings.ToUpper(graphQLErr.Type)
}

// enterpriseGraphQLTransport turns errors GitHub returns inside an HTTP 200
// GraphQL body into gRPC codes. It is not used by the shared GraphQL client,
// because userResourceType.checkOrgSAML matches the text of that client's errors.
type enterpriseGraphQLTransport struct {
	base http.RoundTripper
}

func (t *enterpriseGraphQLTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil
	}

	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("baton-github: error reading the GraphQL response: %w", readErr)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))

	var envelope graphQLEnvelope
	if unmarshalErr := json.Unmarshal(body, &envelope); unmarshalErr != nil {
		// Leave unparseable bodies for the GraphQL library to report.
		return resp, nil //nolint:nilerr // decoding the payload is the library's job
	}
	if len(envelope.Errors) == 0 {
		return resp, nil
	}

	messages := make([]string, 0, len(envelope.Errors))
	for _, graphQLErr := range envelope.Errors {
		messages = append(messages, graphQLErr.Message)
	}
	st := status.New(graphQLErrorsCode(envelope.Errors), strings.Join(messages, "; "))
	if rlDesc, _ := ratelimit.ExtractRateLimitData(resp.StatusCode, &resp.Header); rlDesc != nil {
		if withDetails, detailsErr := st.WithDetails(rlDesc); detailsErr == nil {
			st = withDetails
		}
	}

	return nil, st.Err()
}

// graphQLErrorsCode maps a GraphQL errors array onto a gRPC code. A rate limit
// wins so the SDK retries; any other classified error wins over NOT_FOUND,
// which callers treat as success; otherwise the first classified entry wins.
func graphQLErrorsCode(graphQLErrors []graphQLError) codes.Code {
	code := codes.Internal
	for _, graphQLErr := range graphQLErrors {
		errorType := graphQLErrorType(graphQLErr)

		switch {
		case strings.Contains(errorType, "RATE_LIMIT"), strings.Contains(errorType, "RATELIMIT"):
			return codes.Unavailable
		case errorType == graphQLErrorForbidden && (code == codes.Internal || code == codes.NotFound):
			code = codes.PermissionDenied
		case errorType == graphQLErrorUnauthenticated && (code == codes.Internal || code == codes.NotFound):
			code = codes.Unauthenticated
		// e.g. inviting someone who already administers the enterprise.
		case errorType == graphQLErrorUnprocessable && (code == codes.Internal || code == codes.NotFound):
			code = codes.FailedPrecondition
		case errorType == graphQLErrorNotFound && code == codes.Internal:
			code = codes.NotFound
		}
	}

	return code
}
