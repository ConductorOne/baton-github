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

// githubMaxPageSize is the largest per_page the GitHub REST API accepts.
const githubMaxPageSize = 100

// Endpoint paths, one element per path segment.
func enterpriseInstallationPath(enterprise string) []string {
	return []string{"enterprises", enterprise, "installation"}
}

func consumedLicensesPathParts(enterprise string) []string {
	return []string{"enterprises", enterprise, "consumed-licenses"}
}

// used for endpoints not in the go-github library
// example: https://docs.github.com/en/enterprise-cloud@latest/rest/enterprise-admin/license?apiVersion=2022-11-28#list-enterprise-consumed-licenses
type Client struct {
	*uhttp.BaseHttpClient
	// baseURL is the go-github client's base, so endpoints follow --instance-url.
	baseURL *url.URL
}

func New(client *github.Client) *Client {
	return &Client{
		BaseHttpClient: uhttp.NewBaseHttpClient(client.Client()),
		baseURL:        client.BaseURL,
	}
}

// endpoint resolves path segments against the base URL, escaping each one
// because url.JoinPath treats its arguments as already escaped.
func (c *Client) endpoint(segments ...string) (string, error) {
	if c.baseURL == nil {
		return "", fmt.Errorf("github client has no base URL")
	}

	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		escaped = append(escaped, url.PathEscape(segment))
	}

	return url.JoinPath(c.baseURL.String(), escaped...)
}

// GetEnterpriseInstallation returns this app's installation on one enterprise.
// It needs the app JWT client, not an installation token.
// https://docs.github.com/en/rest/apps/apps#get-an-enterprise-installation-for-the-authenticated-app
func (c *Client) GetEnterpriseInstallation(ctx context.Context, enterprise string) (*AppInstallation, *v2.RateLimitDescription, error) {
	endpoint, err := c.endpoint(enterpriseInstallationPath(enterprise)...)
	if err != nil {
		return nil, nil, fmt.Errorf("error building the enterprise installation URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating request to get the enterprise installation: %w", err)
	}

	var target AppInstallation
	var rateLimitData v2.RateLimitDescription
	res, err := c.Do(req,
		uhttp.WithJSONResponse(&target),
		uhttp.WithRatelimitData(&rateLimitData),
	)
	if err != nil {
		if res != nil {
			defer res.Body.Close()
			logBody(ctx, res.Body)
		}
		return nil, &rateLimitData, fmt.Errorf("error getting the installation of enterprise %s: %w", enterprise, err)
	}

	defer res.Body.Close()

	return &target, &rateLimitData, nil
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
