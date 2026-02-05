package connector

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	cfg "github.com/conductorone/baton-github/pkg/config"
	"github.com/conductorone/baton-github/pkg/customclient"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/cli"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	jwtv5 "github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const githubDotCom = "https://github.com"

// JWT token expires in 10 minutes, so we set it to 9 minutes to leave some buffer.
const jwtExpiryTime = 9 * time.Minute

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
	jwttoken, err := getJWTToken(ghc.AppId, string(ghc.AppPrivatekeyPath))
	if err != nil {
		return nil, err
	}

	appClient, err := newGitHubClient(ctx,
		ghc.InstanceUrl,
		oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: jwttoken},
		),
	)

	if err != nil {
		return nil, err
	}
	installation, err := findInstallation(ctx, appClient, ghc.Org)
	if err != nil {
		return nil, err
	}

	token, err := getInstallationToken(ctx, appClient, installation.GetID())
	if err != nil {
		return nil, err
	}

	jwtts := oauth2.ReuseTokenSource(
		&oauth2.Token{
			AccessToken: jwttoken,
			Expiry:      time.Now().Add(jwtExpiryTime),
		},
		&appJWTTokenRefresher{
			appID:      ghc.AppId,
			privateKey: string(ghc.AppPrivatekeyPath),
		},
	)
	ts := oauth2.ReuseTokenSource(
		&oauth2.Token{
			AccessToken: token.GetToken(),
			Expiry:      token.GetExpiresAt().Time,
		},
		&appTokenRefresher{
			ctx:            ctx,
			instanceURL:    ghc.InstanceUrl,
			installationID: installation.GetID(),
			jwtTokenSource: jwtts,
		},
	)
	// override the appClient with the reuseTokenSource.
	appClient, err = newGitHubClient(ctx,
		ghc.InstanceUrl,
		jwtts,
	)
	if err != nil {
		return nil, err
	}

	ghClient, err := newGitHubClient(ctx, ghc.InstanceUrl, ts)
	if err != nil {
		return nil, err
	}
	graphqlClient, err := newGitHubGraphqlClient(ctx, ghc.InstanceUrl, ts)
	if err != nil {
		return nil, err
	}

	gh := &GitHub{
		client:                   ghClient,
		appClient:                appClient,
		customClient:             customclient.New(ghClient),
		instanceURL:              ghc.InstanceUrl,
		orgs:                     []string{ghc.Org},
		enterprises:              ghc.Enterprises,
		graphqlClient:            graphqlClient,
		orgCache:                 newOrgNameCache(ghClient),
		syncSecrets:              ghc.SyncSecrets,
		omitArchivedRepositories: ghc.OmitArchivedRepositories,
	}
	return gh, nil
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

func loadPrivateKeyFromString(p string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil || (block.Type != "PRIVATE KEY" && block.Type != "RSA PRIVATE KEY") {
		return nil, errors.New("invalid private key PEM format")
	}

	// PKCS8 format
	if block.Type == "PRIVATE KEY" {
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("not an RSA private key")
		}
		return rsaKey, nil
	}

	// PKCS1 format
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func getJWTToken(appID string, privateKey string) (string, error) {
	key, err := loadPrivateKeyFromString(privateKey)
	if err != nil {
		return "", err
	}
	now := time.Now()
	token, err := jwtv5.NewWithClaims(jwtv5.SigningMethodRS256, jwtv5.MapClaims{
		"iat": now.Unix() - 60,                  // issued at
		"exp": now.Add(time.Minute * 10).Unix(), // expires
		"iss": appID,                            // GitHub App ID
	}).SignedString(key)
	if err != nil {
		return "", err
	}
	return token, nil
}

func findInstallation(ctx context.Context, c *github.Client, orgName string) (*github.Installation, error) {
	installation, resp, err := c.Apps.FindOrganizationInstallation(ctx, orgName)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to find installation")
	}
	return installation, nil
}

func getInstallationToken(ctx context.Context, c *github.Client, id int64) (*github.InstallationToken, error) {
	token, resp, err := c.Apps.CreateInstallationToken(ctx, id, &github.InstallationTokenOptions{})
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API error: %s", body)
	}

	return token, nil
}

// appJWTTokenRefresher is used to refresh the app jwt token when it expires.
type appJWTTokenRefresher struct {
	appID      string
	privateKey string
}

func (r *appJWTTokenRefresher) Token() (*oauth2.Token, error) {
	token, err := getJWTToken(r.appID, r.privateKey)
	if err != nil {
		return nil, err
	}

	return &oauth2.Token{
		AccessToken: token,
		Expiry:      time.Now().Add(jwtExpiryTime),
	}, nil
}

type appTokenRefresher struct {
	ctx            context.Context
	jwtTokenSource oauth2.TokenSource
	instanceURL    string
	installationID int64
}

func (r *appTokenRefresher) Token() (*oauth2.Token, error) {
	appClient, err := newGitHubClient(r.ctx,
		r.instanceURL,
		r.jwtTokenSource,
	)
	if err != nil {
		return nil, err
	}

	token, err := getInstallationToken(r.ctx, appClient, r.installationID)
	if err != nil {
		return nil, err
	}
	return &oauth2.Token{
		AccessToken: token.GetToken(),
		Expiry:      token.GetExpiresAt().Time,
	}, nil
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
