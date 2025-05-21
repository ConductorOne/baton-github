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
	"strconv"
	"strings"
	"time"

	cfg "github.com/conductorone/baton-github/pkg/config"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
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
		Annotations: annotations.New(&v2.SkipEntitlementsAndGrants{}),
	}
	resourceTypeOrgRole = &v2.ResourceType{
		Id:          "org_role",
		DisplayName: "Organization Role",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_ROLE},
		Annotations: v1AnnotationsForResourceType("org_role"),
	}
)

type GitHub struct {
	orgs           []string
	client         *github.Client
	appClient      *github.Client
	instanceURL    string
	graphqlClient  *githubv4.Client
	hasSAMLEnabled *bool
	orgCache       *orgNameCache
	syncSecrets    bool
	installationID int64
}

func (gh *GitHub) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncer {
	resourceSyncers := []connectorbuilder.ResourceSyncer{
		orgBuilder(gh.client, gh.appClient, gh.installationID, gh.orgCache, gh.orgs, gh.syncSecrets),
		teamBuilder(gh.client, gh.orgCache),
		userBuilder(gh.client, gh.hasSAMLEnabled, gh.graphqlClient, gh.orgCache),
		repositoryBuilder(gh.client, gh.orgCache),
		orgRoleBuilder(gh.client, gh.orgCache),
	}

	if gh.syncSecrets {
		resourceSyncers = append(resourceSyncers, apiTokenBuilder(gh.client, gh.hasSAMLEnabled, gh.orgCache))
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
	if gh.appClient != nil {
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
	orgLogins := gh.orgs
	if len(orgLogins) > 1 {
		return nil, fmt.Errorf("github-connector: Only one GitHub organization is allowed per group")
	}

	installation, resp, err := gh.appClient.Apps.GetInstallation(ctx, gh.installationID)
	if err != nil {
		return nil, fmt.Errorf("github-connector: failed to retrieve org: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, status.Error(codes.Unauthenticated, "github token is not authorized")
	}

	if len(orgLogins) > 0 && installation.GetAccount().GetLogin() != orgLogins[0] {
		return nil, fmt.Errorf("github-connector: installation id doesn't match the org name")
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
func New(ctx context.Context, ghc *cfg.Github, appKey string) (*GitHub, error) {
	jwttoken, token, err := getClientToken(ctx, ghc, appKey)
	if err != nil {
		return nil, err
	}

	client, err := newGitHubClient(ctx, ghc.InstanceUrl, token)
	if err != nil {
		return nil, err
	}
	graphqlClient, err := newGitHubGraphqlClient(ctx, ghc.InstanceUrl, token)
	if err != nil {
		return nil, err
	}

	var appClient *github.Client
	if jwttoken != "" {
		appClient, err = newGitHubClient(ctx, ghc.InstanceUrl, jwttoken)
		if err != nil {
			return nil, err
		}
	}

	gh := &GitHub{
		client:        client,
		appClient:     appClient,
		instanceURL:   ghc.InstanceUrl,
		orgs:          ghc.Orgs,
		graphqlClient: graphqlClient,
		orgCache:      newOrgNameCache(client),
		syncSecrets:   ghc.SyncSecrets,
	}

	if ghc.InstallationId != "" {
		i, err := strconv.ParseInt(ghc.InstallationId, 10, 64)
		if err != nil {
			return nil, err
		}
		gh.installationID = i
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

// getClientToken returns
// 1. fine-grained personal access tokens if any.
// 2. (JWT token, installation access tokens) if using github app.
func getClientToken(ctx context.Context, ghc *cfg.Github, privateKey string) (string, string, error) {
	if ghc.Token != "" {
		return "", ghc.Token, nil
	}

	key, err := loadPrivateKeyFromString(privateKey)
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	token, err := jwtv5.NewWithClaims(jwtv5.SigningMethodRS256, jwtv5.MapClaims{
		"iat": now.Unix() - 60,                  // issued at
		"exp": now.Add(time.Minute * 10).Unix(), // expires
		"iss": ghc.AppId,                        // GitHub App ID
	}).SignedString(key)
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx,
		http.MethodPost,
		fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", ghc.InstallationId),
		nil,
	)
	if err != nil {
		return "", "", err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("GitHub API error: %s", body)
	}

	var result struct {
		Token string `json:"token"`
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return token, result.Token, err
}
