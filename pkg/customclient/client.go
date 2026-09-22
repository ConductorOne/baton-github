package customclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
)

// githubMaxPageSize is the largest per_page the GitHub REST API accepts; the
// default is 30.
const (
	githubMaxPageSize = 100

	// AppInstallationsPageSize is the per_page this package requests for app
	// installations. A shorter page tells the caller it reached the last one.
	AppInstallationsPageSize = githubMaxPageSize
)

// Endpoint paths, one element per path segment, because endpoint() escapes
// each element it is given.
var (
	appInstallationsPath      = []string{"app", "installations"}
	consumedLicensesPathParts = func(enterprise string) []string {
		return []string{"enterprises", enterprise, "consumed-licenses"}
	}
)

// used for endpoints not in the go-github library
// example: https://docs.github.com/en/enterprise-cloud@latest/rest/enterprise-admin/license?apiVersion=2022-11-28#list-enterprise-consumed-licenses
type Client struct {
	*uhttp.BaseHttpClient
	// baseURL is the go-github client's own base. Keeping it is what makes
	// these endpoints follow --instance-url: on GitHub Enterprise Server
	// WithEnterpriseURLs sets it to https://host/api/v3/, while a literal
	// api.github.com would query GitHub.com instead of the instance.
	baseURL *url.URL
}

func New(client *github.Client) *Client {
	return &Client{
		BaseHttpClient: uhttp.NewBaseHttpClient(client.Client()),
		baseURL:        client.BaseURL,
	}
}

// endpoint resolves path segments against the base URL. Each element is one
// segment: url.JoinPath treats its arguments as already-escaped path, so a raw
// value containing a slash would silently add segments.
//
// Escaping alone is not enough for traversal, because "." and ".." are
// unreserved and survive url.PathEscape, and JoinPath then resolves them — so
// they are rejected outright rather than escaped.
//
// github.NewClient always sets a base URL, so that error only fires on a
// hand-built Client.
func (c *Client) endpoint(segments ...string) (string, error) {
	if c.baseURL == nil {
		return "", fmt.Errorf("github client has no base URL")
	}

	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("path segment %q would traverse the base URL", segment)
		}
		escaped = append(escaped, url.PathEscape(segment))
	}

	return url.JoinPath(c.baseURL.String(), escaped...)
}

// Authenticates with the app's JWT, not with an installation token.
// The go-github installation account is a *User and cannot carry the
// enterprise slug, so the response is decoded through this package's model.
// https://docs.github.com/en/rest/apps/apps#list-installations-for-the-authenticated-app
func (c *Client) ListAppInstallations(ctx context.Context, page int) ([]*AppInstallation, *v2.RateLimitDescription, error) {
	endpoint, err := c.endpoint(appInstallationsPath...)
	if err != nil {
		return nil, nil, fmt.Errorf("error building the app installations URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating request to list app installations: %w", err)
	}

	q := req.URL.Query()
	q.Add("page", strconv.Itoa(page))
	q.Add("per_page", strconv.Itoa(AppInstallationsPageSize))
	req.URL.RawQuery = q.Encode()

	var target []*AppInstallation
	var rateLimitData v2.RateLimitDescription
	res, err := c.Do(req,
		uhttp.WithJSONResponse(&target),
		uhttp.WithRatelimitData(&rateLimitData),
	)

	if err != nil {
		if res != nil {
			logBody(ctx, res.Body)
		}
		return nil, &rateLimitData, fmt.Errorf("error listing app installations: %w", err)
	}

	defer res.Body.Close()

	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		logBody(ctx, res.Body)
		return nil, &rateLimitData, fmt.Errorf("error listing app installations: %s", res.Status)
	}

	return target, &rateLimitData, nil
}

// https://docs.github.com/en/enterprise-cloud@latest/rest/enterprise-admin/license?apiVersion=2022-11-28#list-enterprise-consumed-licenses
func (c *Client) ListEnterpriseConsumedLicenses(ctx context.Context, enterprise string, page int) (*EnterpriseConsumedLicense, *v2.RateLimitDescription, error) {
	endpoint, err := c.endpoint(consumedLicensesPathParts(enterprise)...)
	if err != nil {
		return nil, nil, fmt.Errorf("error building the consumed licenses URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating request to list enterprise consumed licenses: %w", err)
	}

	q := req.URL.Query()
	q.Add("page", strconv.Itoa(page))
	q.Add("per_page", strconv.Itoa(githubMaxPageSize))
	req.URL.RawQuery = q.Encode()

	var target EnterpriseConsumedLicense
	var rateLimitData v2.RateLimitDescription
	res, err := c.Do(req,
		uhttp.WithJSONResponse(&target),
		uhttp.WithRatelimitData(&rateLimitData),
	)

	if err != nil {
		if res != nil {
			logBody(ctx, res.Body)
		}
		return nil, &rateLimitData, fmt.Errorf("error listing enterprise consumed licenses: %w", err)
	}

	defer res.Body.Close()

	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		logBody(ctx, res.Body)
		return nil, &rateLimitData, fmt.Errorf("error listing enterprise consumed licenses: %s", res.Status)
	}

	return &target, &rateLimitData, nil
}
