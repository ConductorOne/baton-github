package connector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	v2 "github.com/ductone/connector-sdk/pb/c1/connector/v2"
	"github.com/ductone/connector-sdk/pkg/annotations"
	"github.com/ductone/connector-sdk/pkg/connectorbuilder"
	"github.com/ductone/connector-sdk/pkg/uhttp"
	"github.com/google/go-github/v41/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ValidAssetDomains = []string{"avatars.githubusercontent.com"}

var (
	resourceTypeOrg = &v2.ResourceType{
		Id:          "org",
		DisplayName: "Org",
		Annotations: v1AnnotationsForResourceType("org"),
	}
	resourceTypeTeam = &v2.ResourceType{
		Id:          "team",
		DisplayName: "Team",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_GROUP},
		Annotations: v1AnnotationsForResourceType("team"),
	}
	resourceTypeRepository = &v2.ResourceType{
		Id:          "repository",
		DisplayName: "Repository",
		Annotations: v1AnnotationsForResourceType("repository"),
	}
	resourceTypeUser = &v2.ResourceType{
		Id:          "user",
		DisplayName: "User",
		Traits: []v2.ResourceType_Trait{
			v2.ResourceType_TRAIT_USER,
		},
		Annotations: v1AnnotationsForResourceType("user"),
	}
)

type Github struct {
	orgs        []string
	client      *github.Client
	instanceURL string
}

func (gh *Github) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncer {
	return []connectorbuilder.ResourceSyncer{
		orgBuilder(gh.client, gh.orgs),
		teamBuilder(gh.client),
		userBuilder(gh.client),
		repositoryBuilder(gh.client),
	}
}

// validateAssetUrl takes an input URL and validates that it is a URL that we are permitted to fetch assets from/
// It enforces https and that the URL hostname is.
func (gh *Github) validateAssetUrl(assetUrl string) error {
	if assetUrl == "" {
		return fmt.Errorf("asset url must be set")
	}

	parsedUrl, err := url.Parse(assetUrl)
	if err != nil {
		return err
	}

	if parsedUrl.Scheme != "https" {
		return fmt.Errorf("asset url must be https")
	}

	if gh.instanceURL == "" {
		for _, domain := range ValidAssetDomains {
			if strings.HasPrefix(parsedUrl.Hostname(), domain) {
				return nil
			}
		}
	} else {
		parsedInstance, err := url.Parse(gh.instanceURL)
		if err != nil {
			return err
		}

		if strings.HasSuffix(parsedUrl.Hostname(), parsedInstance.Hostname()) {
			return nil
		}
	}

	return fmt.Errorf("invalid asset url")
}

// GetAsset takes an input AssetRef and attempts to fetch it using the connector's authenticated http client
// It streams a response, always starting with a metadata object, following by chunked payloads for the asset.
func (gh *Github) Asset(ctx context.Context, asset *v2.AssetRef) (string, io.ReadCloser, error) {
	if asset == nil {
		return "", nil, fmt.Errorf("asset must be provided")
	}
	err := gh.validateAssetUrl(asset.Id)
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.GetId(), nil)
	if err != nil {
		return "", nil, err
	}
	resp, err := gh.client.Client().Do(req)
	if err != nil {
		return "", nil, err
	}

	return resp.Header.Get("Content-Type"), resp.Body, nil
}

// Metadata returns metadata about the connector.
func (gh *Github) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
	return &v2.ConnectorMetadata{
		DisplayName: "Github",
	}, nil
}

// Validate hits the Github API to validate that the configured credentials are still valid.
func (gh *Github) Validate(ctx context.Context) (annotations.Annotations, error) {
	page := 0
	orgLogins := gh.orgs
	filterOrgs := true

	if len(orgLogins) == 0 {
		filterOrgs = false
		for {
			orgs, resp, err := gh.client.Organizations.List(ctx, "", &github.ListOptions{Page: page})
			if resp.StatusCode == http.StatusUnauthorized {
				return nil, status.Error(codes.Unauthenticated, "github token is not authorized")
			}
			if err != nil {
				return nil, fmt.Errorf("github-connector: failed to retrieve org: %w", err)
			}

			for _, o := range orgs {
				orgLogins = append(orgLogins, o.GetLogin())
			}

			if resp.NextPage == 0 {
				break
			}

			page = resp.NextPage
		}
	}

	adminFound := false
	for _, o := range orgLogins {
		membership, _, err := gh.client.Organizations.GetOrgMembership(ctx, "", o)
		if err != nil {
			if filterOrgs {
				return nil, fmt.Errorf("access token must be an admin on the %s organization", o)
			}
			continue
		}

		// Only sync orgs that we are an admin for
		if strings.ToLower(membership.GetRole()) != orgRoleAdmin {
			if filterOrgs {
				return nil, fmt.Errorf("access token must be an admin on the %s organization", o)
			}
			continue
		}

		adminFound = true
	}

	if !adminFound {
		return nil, fmt.Errorf("access token must be an admin on at least one organization")
	}

	return nil, nil
}

// newGithubClient returns a new github API client authenticated with an access token via oauth2.
func newGithubClient(ctx context.Context, instanceURL string, accessToken string) (*github.Client, error) {
	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: accessToken},
	)
	tc := oauth2.NewClient(ctx, ts)

	if instanceURL != "" {
		return github.NewEnterpriseClient(instanceURL, instanceURL, tc)
	}

	return github.NewClient(tc), nil
}

// New returns the v2 version of the github connector.
func New(ctx context.Context, githubOrgs []string, instanceURL, accessToken string) (*Github, error) {
	client, err := newGithubClient(ctx, instanceURL, accessToken)
	if err != nil {
		return nil, err
	}
	gh := &Github{
		client:      client,
		instanceURL: instanceURL,
		orgs:        githubOrgs,
	}

	return gh, nil
}
