package connector

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	cfg "github.com/conductorone/baton-github/pkg/config"
	"github.com/conductorone/baton-github/pkg/customclient"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/cli"
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

var (
	ValidAssetDomains     = []string{"avatars.githubusercontent.com"}
	maxPageSize       int = 100 // maximum page size github supported.
)

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
	resourceTypeInvitation = &v2.ResourceType{
		Id:          "invitation",
		DisplayName: "Invitation",
		Traits: []v2.ResourceType_Trait{
			v2.ResourceType_TRAIT_USER,
		},
		Annotations: v1AnnotationsForResourceType("invitation"),
	}
	resourceTypeApiToken = &v2.ResourceType{
		Id:          "api-key",
		DisplayName: "API Key",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_SECRET},
		Annotations: annotations.New(&v2.SkipEntitlementsAndGrants{}),
	}
	resourceTypeOrgRole = &v2.ResourceType{
		Id:          "org_role",
		DisplayName: "Organization Role",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_ROLE},
		Annotations: v1AnnotationsForResourceType("org_role"),
	}
	resourceTypeEnterpriseRole = &v2.ResourceType{
		Id:          "enterprise_role",
		DisplayName: "Enterprise Role",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_ROLE},
		Annotations: v1AnnotationsForResourceType("enterprise_role"),
	}
)

type GitHub struct {
	orgs                     []string
	client                   *github.Client
	appClient                *github.Client
	customClient             *customclient.Client
	instanceURL              string
	graphqlClient            *githubv4.Client
	hasSAMLEnabled           *bool
	orgCache                 *orgNameCache
	syncSecrets              bool
	omitArchivedRepositories bool
	enterprises              []string
}

func (gh *GitHub) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncerV2 {
	resourceSyncers := []connectorbuilder.ResourceSyncerV2{
		orgBuilder(gh.client, gh.appClient, gh.orgCache, gh.orgs, gh.syncSecrets),
		teamBuilder(gh.client, gh.orgCache),
		userBuilder(gh.client, gh.hasSAMLEnabled, gh.graphqlClient, gh.orgCache, gh.orgs),
		repositoryBuilder(gh.client, gh.orgCache, gh.omitArchivedRepositories),
		orgRoleBuilder(gh.client, gh.orgCache),
		invitationBuilder(invitationBuilderParams{
			client:   gh.client,
			orgCache: gh.orgCache,
			orgs:     gh.orgs,
		}),
	}

	if gh.syncSecrets {
		resourceSyncers = append(resourceSyncers, apiTokenBuilder(gh.client, gh.hasSAMLEnabled, gh.orgCache))
	}

	if len(gh.enterprises) > 0 {
		resourceSyncers = append(resourceSyncers, enterpriseRoleBuilder(gh.client, gh.customClient, gh.enterprises))
	}
	return resourceSyncers
}

// Metadata returns metadata about the connector.
func (gh *GitHub) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
	return &v2.ConnectorMetadata{
		DisplayName: "GitHub",
		AccountCreationSchema: &v2.ConnectorAccountCreationSchema{
			FieldMap: map[string]*v2.ConnectorAccountCreationSchema_Field{
				"email": {
					DisplayName: "Email",
					Required:    true,
					Description: "This email will be used as the login for the user.",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "Email",
					Order:       1,
				},
				"org": {
					DisplayName: "Org Name",
					Required:    true,
					Description: "organization name",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "organization name",
					Order:       2,
				},
			},
		},
	}, nil
}

// Validate hits the GitHub API to validate that the configured credentials are still valid.
func (gh *GitHub) Validate(ctx context.Context) (annotations.Annotations, error) {
	if gh.appClient != nil {
		return gh.validateAppCredentials(ctx)
	}

	orgLogins := gh.orgs
	filterOrgs := true

	if len(orgLogins) == 0 {
		filterOrgs = false

		var err error
		orgLogins, err = getOrgs(ctx, gh.client, orgLogins)
		if err != nil {
			return nil, err
		}
	}

	adminFound := false
	for _, o := range orgLogins {
		membership, _, err := gh.client.Organizations.GetOrgMembership(ctx, "", o)
		if err != nil {
			if filterOrgs {
				err := fmt.Errorf("can't get authenticated user on the %s organization: %w", o, err)
				return nil, uhttp.WrapErrors(codes.PermissionDenied, "github-connector: credentials validation failed", err)
			}
			continue
		}

		// Only sync orgs that we are an admin for
		if strings.ToLower(membership.GetRole()) != orgRoleAdmin {
			if filterOrgs {
				err := fmt.Errorf("access token must be an admin on the %s organization", o)
				return nil, uhttp.WrapErrors(codes.PermissionDenied, "github-connector: credentials validation failed", err)
			}
			continue
		}

		adminFound = true
	}

	if !adminFound {
		err := fmt.Errorf("access token must be an admin on at least one organization")
		return nil, uhttp.WrapErrors(codes.PermissionDenied, "github-connector: credentials validation failed", err)
	}

	if len(gh.enterprises) > 0 {
		_, _, err := gh.customClient.ListEnterpriseConsumedLicenses(ctx, gh.enterprises[0], 0)
		if err != nil {
			return nil, uhttp.WrapErrors(codes.PermissionDenied, "github-connector: failed to access enterprise licenses", err)
		}
	}
	return nil, nil
}

func (gh *GitHub) validateAppCredentials(ctx context.Context) (annotations.Annotations, error) {
	orgLogins := gh.orgs
	if len(orgLogins) > 1 {
		return nil, fmt.Errorf("github-connector: only one org is allowed when using github app")
	}

	_, err := findInstallation(ctx, gh.appClient, orgLogins[0])
	if err != nil {
		return nil, err
	}
	return nil, nil
}

// newGitHubClient returns a new GitHub API client authenticated with an access token via oauth2.
func newGitHubClient(ctx context.Context, instanceURL string, ts oauth2.TokenSource) (*github.Client, error) {
	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)

	tc := oauth2.NewClient(ctx, ts)
	gc := github.NewClient(tc)

	instanceURL = strings.TrimSuffix(instanceURL, "/")
	if instanceURL != "" && instanceURL != githubDotCom {
		return gc.WithEnterpriseURLs(instanceURL, instanceURL)
	}

	return gc, nil
}

func NewLambdaConnector(ctx context.Context, ghc *cfg.Github, cliOpts *cli.ConnectorOpts) (connectorbuilder.ConnectorBuilderV2, []connectorbuilder.Opt, error) {
	var (
		group = cliOpts.SelectedAuthMethod
		cb    *GitHub
		err   error
	)
	if group == cfg.GithubAppGroup {
		cb, err = newWithGithubApp(ctx, ghc)
		if err != nil {
			return nil, nil, err
		}
		return cb, nil, nil
	}

	cb, err = newWithGithubPAT(ctx, ghc)
	if err != nil {
		return nil, nil, err
	}
	return cb, nil, nil
}

func newWithGithubPAT(ctx context.Context, ghc *cfg.Github) (*GitHub, error) {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: ghc.Token},
	)
	ghClient, err := newGitHubClient(ctx, ghc.InstanceUrl, ts)
	if err != nil {
		return nil, err
	}
	graphqlClient, err := newGitHubGraphqlClient(ctx, ghc.InstanceUrl, ts)
	if err != nil {
		return nil, err
	}
	return &GitHub{
		client:                   ghClient,
		customClient:             customclient.New(ghClient),
		instanceURL:              ghc.InstanceUrl,
		orgs:                     ghc.Orgs,
		enterprises:              ghc.Enterprises,
		graphqlClient:            graphqlClient,
		orgCache:                 newOrgNameCache(ghClient),
		syncSecrets:              ghc.SyncSecrets,
		omitArchivedRepositories: ghc.OmitArchivedRepositories,
	}, nil
}

func newWithGithubApp(ctx context.Context, ghc *cfg.Github) (*GitHub, error) {
	if len(ghc.Orgs) != 1 {
		return nil, fmt.Errorf("github-connector: only one org should be specified for GitHub App authentication")
	}

	// Parse App ID
	appID, err := strconv.ParseInt(ghc.AppId, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("github-connector: invalid app-id: %w", err)
	}

	// Get the private key content
	appKey := string(ghc.AppPrivatekeyPath)

	// Create app transport for finding installation
	appTransport, err := ghinstallation.NewAppsTransport(
		http.DefaultTransport,
		appID,
		[]byte(appKey),
	)
	if err != nil {
		return nil, fmt.Errorf("github-connector: failed to create app transport: %w", err)
	}

	// Set base URL for GitHub Enterprise
	instanceURL := strings.TrimSuffix(ghc.InstanceUrl, "/")
	if instanceURL != "" && instanceURL != githubDotCom {
		appTransport.BaseURL = instanceURL + "/api/v3"
	}

	appClient := github.NewClient(&http.Client{Transport: appTransport})
	if instanceURL != "" && instanceURL != githubDotCom {
		appClient, err = appClient.WithEnterpriseURLs(instanceURL, instanceURL)
		if err != nil {
			return nil, fmt.Errorf("github-connector: failed to set enterprise URLs for app client: %w", err)
		}
	}

	// Find installation for the org
	installation, err := findInstallation(ctx, appClient, ghc.Orgs[0])
	if err != nil {
		return nil, err
	}

	// Create installation transport - handles all token refresh automatically
	installTransport, err := ghinstallation.New(
		http.DefaultTransport,
		appID,
		installation.GetID(),
		[]byte(appKey),
	)
	if err != nil {
		return nil, fmt.Errorf("github-connector: failed to create installation transport: %w", err)
	}

	// Set base URL for GitHub Enterprise
	if instanceURL != "" && instanceURL != githubDotCom {
		installTransport.BaseURL = instanceURL + "/api/v3"
	}

	ghClient := github.NewClient(&http.Client{Transport: installTransport})
	if instanceURL != "" && instanceURL != githubDotCom {
		ghClient, err = ghClient.WithEnterpriseURLs(instanceURL, instanceURL)
		if err != nil {
			return nil, fmt.Errorf("github-connector: failed to set enterprise URLs for install client: %w", err)
		}
	}

	// Wrap for GraphQL client which needs oauth2.TokenSource
	ts := &ghinstallationTokenSource{transport: installTransport}
	graphqlClient, err := newGitHubGraphqlClient(ctx, ghc.InstanceUrl, ts)
	if err != nil {
		return nil, err
	}

	return &GitHub{
		client:                   ghClient,
		appClient:                appClient,
		customClient:             customclient.New(ghClient),
		instanceURL:              ghc.InstanceUrl,
		orgs:                     ghc.Orgs,
		enterprises:              ghc.Enterprises,
		graphqlClient:            graphqlClient,
		orgCache:                 newOrgNameCache(ghClient),
		syncSecrets:              ghc.SyncSecrets,
		omitArchivedRepositories: ghc.OmitArchivedRepositories,
	}, nil
}

func newGitHubGraphqlClient(ctx context.Context, instanceURL string, ts oauth2.TokenSource) (*githubv4.Client, error) {
	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)

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

func findInstallation(ctx context.Context, c *github.Client, orgName string) (*github.Installation, error) {
	installation, resp, err := c.Apps.FindOrganizationInstallation(ctx, orgName)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to find installation")
	}
	return installation, nil
}

// ghinstallationTokenSource wraps ghinstallation.Transport to implement oauth2.TokenSource
// for use with the GraphQL client.
type ghinstallationTokenSource struct {
	transport *ghinstallation.Transport
}

func (g *ghinstallationTokenSource) Token() (*oauth2.Token, error) {
	token, err := g.transport.Token(context.Background())
	if err != nil {
		return nil, err
	}
	// Use actual token expiry from ghinstallation transport.
	// If Expiry() fails, fallback to forcing re-evaluation by returning
	// time.Now() which will cause oauth2 to refresh the token.
	expiresAt, _, expiryErr := g.transport.Expiry()
	if expiryErr != nil {
		//nolint:nilerr // Intentional: gracefully degrade when expiry unavailable
		return &oauth2.Token{AccessToken: token, Expiry: time.Now()}, nil
	}
	return &oauth2.Token{AccessToken: token, Expiry: expiresAt}, nil
}

func getOrgs(ctx context.Context, client *github.Client, orgs []string) ([]string, error) {
	if len(orgs) != 0 {
		return orgs, nil
	}

	var (
		page      = 0
		orgLogins []string
	)
	for {
		orgs, resp, err := client.Organizations.List(ctx, "", &github.ListOptions{Page: page, PerPage: maxPageSize})
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to retrieve organizations")
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
	return orgLogins, nil
}
