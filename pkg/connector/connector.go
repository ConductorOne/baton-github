package connector

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const githubDotCom = "https://github.com"

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
	resourceTypeApiToken = &v2.ResourceType{
		Id:          "api-key",
		DisplayName: "API Key",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_SECRET},
		Annotations: annotationsForResourceType(true),
	}
)

func annotationsForResourceType(skipEntitlementsAndGrants bool) annotations.Annotations {
	annos := annotations.Annotations{}

	if skipEntitlementsAndGrants {
		annos.Update(&v2.SkipEntitlementsAndGrants{})
	}

	return annos
}

type GitHub struct {
	orgs           []string
	client         *github.Client
	instanceURL    string
	graphqlClient  *githubv4.Client
	hasSAMLEnabled *bool
	orgCache       *orgNameCache
	syncSecrets    bool
}

func (gh *GitHub) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncer {
	resourceSyncers := []connectorbuilder.ResourceSyncer{
		orgBuilder(gh.client, gh.orgCache, gh.orgs),
		teamBuilder(gh.client, gh.orgCache),
		userBuilder(gh.client, gh.hasSAMLEnabled, gh.graphqlClient, gh.orgCache),
		repositoryBuilder(gh.client, gh.orgCache),
	}

	if gh.syncSecrets {
		resourceSyncers = append(resourceSyncers, apiTokenBuilder(gh.client, gh.hasSAMLEnabled, gh.graphqlClient, gh.orgCache))
	}
	return resourceSyncers
}

// Metadata returns metadata about the connector.
func (gh *GitHub) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
	return &v2.ConnectorMetadata{
		DisplayName: "GitHub",
	}, nil
}

// Validate hits the GitHub API to validate that the configured credentials are still valid.
func (gh *GitHub) Validate(ctx context.Context) (annotations.Annotations, error) {
	page := 0
	orgLogins := gh.orgs
	filterOrgs := true

	if len(orgLogins) == 0 {
		filterOrgs = false
		for {
			orgs, resp, err := gh.client.Organizations.List(ctx, "", &github.ListOptions{Page: page})
			if err != nil {
				return nil, fmt.Errorf("github-connector: failed to retrieve org: %w", err)
			}
			if resp.StatusCode == http.StatusUnauthorized {
				return nil, status.Error(codes.Unauthenticated, "github token is not authorized")
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

// newGitHubClient returns a new GitHub API client authenticated with an access token via oauth2.
func newGitHubClient(ctx context.Context, instanceURL string, accessToken string) (*github.Client, error) {
	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: accessToken},
	)
	tc := oauth2.NewClient(ctx, ts)
	gc := github.NewClient(tc)

	instanceURL = strings.TrimSuffix(instanceURL, "/")
	if instanceURL != "" && instanceURL != githubDotCom {
		return gc.WithEnterpriseURLs(instanceURL, instanceURL)
	}

	return gc, nil
}

// New returns the GitHub connector configured to sync against the instance URL.
func New(ctx context.Context, githubOrgs []string, instanceURL, accessToken string, syncSecrets bool) (*GitHub, error) {
	client, err := newGitHubClient(ctx, instanceURL, accessToken)
	if err != nil {
		return nil, err
	}
	graphqlClient, err := newGitHubGraphqlClient(ctx, instanceURL, accessToken)
	if err != nil {
		return nil, err
	}
	gh := &GitHub{
		client:        client,
		instanceURL:   instanceURL,
		orgs:          githubOrgs,
		graphqlClient: graphqlClient,
		orgCache:      newOrgNameCache(client),
		syncSecrets:   syncSecrets,
	}

	return gh, nil
}

func newGitHubGraphqlClient(ctx context.Context, instanceURL string, accessToken string) (*githubv4.Client, error) {
	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: accessToken},
	)
	tc := oauth2.NewClient(ctx, ts)

	instanceURL = strings.TrimSuffix(instanceURL, "/")
	if instanceURL != "" && instanceURL != githubDotCom {
		gqlURL, err := url.Parse(instanceURL)
		if err != nil {
			return nil, err
		}

		gqlURL.Path = "/api/graphql"

		return githubv4.NewEnterpriseClient(gqlURL.String(), tc), nil
	}

	return githubv4.NewClient(tc), nil
}
