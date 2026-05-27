package connector

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/shurcooL/githubv4"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

// refreshableTokenSource caches an oauth2 token and exposes Invalidate() so
// the cached value can be marked dead in response to a server-side signal
// (e.g. a 401 from GitHub). Compared to oauth2.ReuseTokenSource, this allows
// recovery from server-side token rotation that happens before the cached
// token's stated Expiry — see inc-814.
type refreshableTokenSource struct {
	mu     sync.Mutex
	cached *oauth2.Token
	source oauth2.TokenSource
}

func newRefreshableTokenSource(initial *oauth2.Token, source oauth2.TokenSource) *refreshableTokenSource {
	return &refreshableTokenSource{cached: initial, source: source}
}

func (r *refreshableTokenSource) Token() (*oauth2.Token, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached.Valid() {
		return r.cached, nil
	}
	tok, err := r.source.Token()
	if err != nil {
		return nil, err
	}
	r.cached = tok
	return tok, nil
}

// Invalidate clears the cached token. The next Token() call forces a fresh
// fetch from the underlying source.
func (r *refreshableTokenSource) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cached = nil
}

// tokenRefreshTransport observes 401 responses, invalidates the wrapped token
// source, and retries the request exactly once with the freshly-issued token.
// Requests with a non-rewindable body (non-nil Body, nil GetBody) are returned
// with their original 401 unchanged — the only safe move is to surface it.
type tokenRefreshTransport struct {
	next http.RoundTripper
	rts  *refreshableTokenSource
}

func (t *tokenRefreshTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	l := ctxzap.Extract(req.Context())

	if req.Body != nil && req.GetBody == nil {
		l.Debug("skipping 401 retry: request body not rewindable (no GetBody)",
			zap.String("method", req.Method),
			zap.String("url", req.URL.String()),
		)
		return resp, nil
	}

	var newBody io.ReadCloser
	if req.GetBody != nil {
		body, gbErr := req.GetBody()
		if gbErr != nil {
			l.Warn("skipping 401 retry: GetBody failed",
				zap.Error(gbErr),
				zap.String("method", req.Method),
			)
			return resp, nil
		}
		newBody = body
	}

	// Drain and close the 401 body so the underlying connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	t.rts.Invalidate()
	if newBody != nil {
		req.Body = newBody
	}

	l.Debug("retrying request after 401 with refreshed token",
		zap.String("method", req.Method),
		zap.String("url", req.URL.String()),
	)
	return t.next.RoundTrip(req)
}

// newGitHubAppHTTPClient builds the layered transport chain for GitHub App
// auth: uhttp logging at the bottom, oauth2 auth-header injection in the
// middle, 401-retry middleware on top. It bypasses oauth2.NewClient (which
// would re-wrap rts in its own ReuseTokenSource cache that doesn't observe
// our Invalidate calls).
func newGitHubAppHTTPClient(ctx context.Context, rts *refreshableTokenSource) (*http.Client, error) {
	base, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout: base.Timeout,
		Transport: &tokenRefreshTransport{
			next: &oauth2.Transport{
				Base:   base.Transport,
				Source: rts,
			},
			rts: rts,
		},
	}, nil
}

// newGitHubAppClients constructs the REST and GraphQL clients used by the
// GitHub App install-token path, sharing the layered httpClient so the
// 401-refresh middleware applies to both surfaces.
//
// The GraphQL client gets an extra statusClassifyingTransport layered on top of
// the shared transport: shurcooL/githubv4 surfaces non-2xx responses as opaque
// strings, so without it transient errors (429, 5xx) would reach the SDK as
// codes.Unknown and abort the sync instead of being retried. It must sit above
// the 401-retry layer so tokenRefreshTransport still observes a raw 401 and can
// retry it; only a non-2xx that survives the retry is converted to a classified
// error. The REST client doesn't need this — go-github exposes structured
// errors that wrapGitHubError classifies at the call site.
func newGitHubAppClients(instanceURL string, httpClient *http.Client) (*github.Client, *githubv4.Client, error) {
	instanceURL = strings.TrimSuffix(instanceURL, "/")

	gc := github.NewClient(httpClient)
	if instanceURL != "" && instanceURL != githubDotCom {
		var err error
		gc, err = gc.WithEnterpriseURLs(instanceURL, instanceURL)
		if err != nil {
			return nil, nil, err
		}
	}

	gqlHTTPClient := &http.Client{
		Timeout:   httpClient.Timeout,
		Transport: &statusClassifyingTransport{base: httpClient.Transport},
	}

	var gqlClient *githubv4.Client
	if instanceURL != "" && instanceURL != githubDotCom {
		gqlURL, err := url.Parse(instanceURL)
		if err != nil {
			return nil, nil, err
		}
		gqlURL.Path = "/api/graphql"
		gqlClient = githubv4.NewEnterpriseClient(gqlURL.String(), gqlHTTPClient)
	} else {
		gqlClient = githubv4.NewClient(gqlHTTPClient)
	}
	return gc, gqlClient, nil
}
