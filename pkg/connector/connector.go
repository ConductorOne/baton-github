package connector

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	cfg "github.com/conductorone/baton-github/pkg/config"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	jwtv5 "github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v63/github"
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
)

type GitHub struct {
	orgs           []string
	client         *github.Client
	instanceURL    string
	graphqlClient  *githubv4.Client
	hasSAMLEnabled *bool
	orgCache       *orgNameCache
	app            gitHubApp
}

// gitHubApp has app clients that are used by the GitHub App.
// Each app has a single JWT token shared across all organizations.
// However, each organization has its own unique installation token.
type gitHubApp struct {
	appJWTClient          *github.Client
	appInstallationClient map[int64]*github.Client
	graphqlClient         map[int64]*githubv4.Client
}

func (gh *GitHub) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncer {
	return []connectorbuilder.ResourceSyncer{
		orgBuilder(gh.client, gh.app, gh.orgCache, gh.orgs),
		teamBuilder(gh.client, gh.app, gh.orgCache),
		userBuilder(gh.client, gh.app, gh.hasSAMLEnabled, gh.graphqlClient, gh.orgCache),
		repositoryBuilder(gh.client, gh.app, gh.orgCache),
	}
}

// Metadata returns metadata about the connector.
func (gh *GitHub) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
	return &v2.ConnectorMetadata{
		DisplayName: "GitHub",
	}, nil
}

// Validate hits the GitHub API to validate that the configured credentials are still valid.
func (gh *GitHub) Validate(ctx context.Context) (annotations.Annotations, error) {
	if gh.app.appJWTClient != nil {
		return gh.validateAppCredentials(ctx)
	}

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

func (gh *GitHub) validateAppCredentials(ctx context.Context) (annotations.Annotations, error) {
	iResp, err := getAllInstallations(ctx, gh.app.appJWTClient)
	if err != nil {
		return nil, err
	}

	for _, o := range gh.orgs {
		if _, ok := iResp.accountNames[o]; !ok {
			return nil, fmt.Errorf("access token must be an admin on the %s organization", o)
		}
	}
	return nil, nil
}

type getAllInstallationsResp struct {
	installationIds map[int64]*github.Installation
	accountIds      map[int64]struct{}
	accountNames    map[string]struct{}
}

func getAllInstallations(ctx context.Context, c *github.Client) (getAllInstallationsResp, error) {
	var (
		AccountIDMap                  = make(map[int64]struct{})
		installationsIDToInstallation = make(map[int64]*github.Installation)
		AccountNameMap                = make(map[string]struct{})
		page                          = 0
	)
	for {
		installations, resp, err := c.Apps.ListInstallations(ctx, &github.ListOptions{Page: page})
		if err != nil {
			return getAllInstallationsResp{}, fmt.Errorf("github-connector: failed to retrieve org: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return getAllInstallationsResp{}, status.Error(codes.Unauthenticated, "github token is not authorized")
		}
		for _, installation := range installations {
			if installation.GetAccount().GetType() != "Organization" {
				continue
			}
			installationsIDToInstallation[installation.GetID()] = installation
			AccountIDMap[installation.GetAccount().GetID()] = struct{}{}
			AccountNameMap[installation.GetAccount().GetLogin()] = struct{}{}
		}

		if resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return getAllInstallationsResp{
		installationIds: installationsIDToInstallation,
		accountIds:      AccountIDMap,
		accountNames:    AccountNameMap,
	}, nil
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
func New(ctx context.Context, ghc *cfg.Github) (*GitHub, error) {
	if ghc.Token == "" {
		app, err := newAppClient(ctx, ghc)
		if err != nil {
			return nil, err
		}
		return &GitHub{
			instanceURL: ghc.InstanceUrl,
			orgs:        ghc.Orgs,
			app:         app,
			orgCache:    newOrgNameCache(nil, app.appInstallationClient),
		}, nil
	}

	client, err := newGitHubClient(ctx, ghc.InstanceUrl, ghc.Token)
	if err != nil {
		return nil, err
	}
	graphqlClient, err := newGitHubGraphqlClient(ctx, ghc.InstanceUrl, ghc.Token)
	if err != nil {
		return nil, err
	}
	gh := &GitHub{
		client:        client,
		instanceURL:   ghc.InstanceUrl,
		orgs:          ghc.Orgs,
		graphqlClient: graphqlClient,
		orgCache:      newOrgNameCache(client, nil),
	}

	return gh, nil
}

func newAppClient(ctx context.Context, ghc *cfg.Github) (gitHubApp, error) {
	key, err := loadPrivateKeyFromString(ghc.AppPrivatekey)
	if err != nil {
		return gitHubApp{}, err
	}
	now := time.Now()
	token, err := jwtv5.NewWithClaims(jwtv5.SigningMethodRS256, jwtv5.MapClaims{
		"iat": now.Unix() - 60,                  // issued at
		"exp": now.Add(time.Minute * 10).Unix(), // expires
		"iss": ghc.AppId,                        // GitHub App ID
	}).SignedString(key)
	if err != nil {
		return gitHubApp{}, err
	}

	client, err := newGitHubClient(ctx, ghc.InstanceUrl, token)
	if err != nil {
		return gitHubApp{}, err
	}

	iResp, err := getAllInstallations(ctx, client)
	if err != nil {
		return gitHubApp{}, err
	}

	var (
		installationsClient   = make(map[int64]*github.Client, len(iResp.installationIds))
		installationsGLClient = make(map[int64]*githubv4.Client, len(iResp.accountIds))
	)
	for id, installation := range iResp.installationIds {
		c, glc, err := getInstallationClient(ctx, ghc.InstanceUrl, id, token)
		if err != nil {
			return gitHubApp{}, err
		}
		installationsClient[installation.GetAccount().GetID()] = c
		installationsGLClient[installation.GetAccount().GetID()] = glc
	}
	return gitHubApp{
		appJWTClient:          client,
		appInstallationClient: installationsClient,
		graphqlClient:         installationsGLClient,
	}, nil
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

func getInstallationClient(ctx context.Context, instanceURL string, orgId int64, jwtToken string) (*github.Client, *githubv4.Client, error) {
	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", orgId)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("GitHub API error: %s", body)
	}

	var result struct {
		Token string `json:"token"`
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return nil, nil, err
	}

	client, err := newGitHubClient(ctx, instanceURL, result.Token)
	if err != nil {
		return nil, nil, err
	}
	gcclient, err := newGitHubGraphqlClient(ctx, instanceURL, result.Token)
	if err != nil {
		return nil, nil, err
	}
	return client, gcclient, nil
}
