package connector

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v41/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
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

	instanceURL = strings.TrimSuffix(instanceURL, "/")
	if instanceURL != "" && instanceURL != "https://github.com" {
		return github.NewEnterpriseClient(instanceURL, instanceURL, tc)
	}

	return github.NewClient(tc), nil
}

// New returns the GitHub connector configured to sync against the instance URL.
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
