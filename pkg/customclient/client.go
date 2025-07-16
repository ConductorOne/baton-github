package customclient

import (
	"context"
	"fmt"
	"net/http"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
)

// used for endpoints not in the go-github library
// example: https://docs.github.com/en/enterprise-cloud@latest/rest/enterprise-admin/license?apiVersion=2022-11-28#list-enterprise-consumed-licenses
type Client struct {
	*uhttp.BaseHttpClient
}

func New(client *github.Client) *Client {
	return &Client{
		BaseHttpClient: uhttp.NewBaseHttpClient(client.Client()),
	}
}

// https://docs.github.com/en/enterprise-cloud@latest/rest/enterprise-admin/license?apiVersion=2022-11-28#list-enterprise-consumed-licenses
func (c *Client) ListEnterpriseConsumedLicenses(ctx context.Context, enterprise string, page int) (*EnterpriseConsumedLicense, *v2.RateLimitDescription, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://api.github.com/enterprises/%s/consumed-licenses", enterprise), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating request to list enterprise consumed licenses: %w", err)
	}

	q := req.URL.Query()
	q.Add("page", fmt.Sprintf("%d", page))
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
