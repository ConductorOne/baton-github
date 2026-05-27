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
	"os"
	"strconv"
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
	"go.uber.org/zap"
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
		Annotations: skipEntitlementsAnnotations("org"),
	}
	resourceTypeTeam = &v2.ResourceType{
		Id:          "team",
		DisplayName: "Team",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_GROUP},
		Annotations: skipEntitlementsAnnotations("team"),
	}
	resourceTypeRepository = &v2.ResourceType{
		Id:          "repository",
		DisplayName: "Repository",
		Annotations: skipEntitlementsAnnotations("repository"),
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
		// Invitations emit TRAIT_USER with UserTrait_Status_STATUS_UNSPECIFIED.
		// Accepted members from user.go emit STATUS_ENABLED.
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
		Annotations: skipEntitlementsAnnotations("org_role"),
	}
	resourceTypeEnterpriseRole = &v2.ResourceType{
		Id:          "enterprise_role",
		DisplayName: "Enterprise Role",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_ROLE},
		Annotations: skipEntitlementsAnnotations("enterprise_role"),
	}
)

type GitHub struct {
	orgs                     []string
	client                   *github.Client
	appClient                *github.Client
	customClient             *customclient.Client
	instanceURL              string
	graphqlClient            *githubv4.Client
	orgCache                 *orgNameCache
	syncSecrets              bool
	omitArchivedRepositories bool
	directCollaboratorsOnly  bool
	enterprises              []string
	// etagTransports holds references to any ETagTransport instances installed
	// by newETagWrap, kept so Close can emit a sync-end stats summary.
	etagTransports []*customclient.ETagTransport
}

// Close implements connectorbuilder.closeWithContext: emits the ETag-cache hit
// rate so operators can see whether the cache is doing useful work.
func (gh *GitHub) Close(ctx context.Context) error {
	for _, t := range gh.etagTransports {
		t.LogStats(ctx)
	}
	return nil
}

func (gh *GitHub) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncerV2 {
	resourceSyncers := []connectorbuilder.ResourceSyncerV2{
		orgBuilder(gh.client, gh.appClient, gh.orgCache, gh.orgs, gh.syncSecrets),
		teamBuilder(gh.client, gh.orgCache, gh.directCollaboratorsOnly),
		userBuilder(gh.client, gh.graphqlClient, gh.orgCache, gh.orgs, gh.customClient, gh.enterprises),
		repositoryBuilder(gh.client, gh.orgCache, gh.omitArchivedRepositories, gh.directCollaboratorsOnly),
		orgRoleBuilder(gh.client, gh.orgCache),
		invitationBuilder(invitationBuilderParams{
			client:   gh.client,
			orgCache: gh.orgCache,
			orgs:     gh.orgs,
		}),
	}

	if gh.syncSecrets {
		resourceSyncers = append(resourceSyncers, apiTokenBuilder(gh.client, gh.orgCache))
	}

	if len(gh.enterprises) > 0 {
		resourceSyncers = append(resourceSyncers, enterpriseRoleBuilder(gh.client, gh.appClient, gh.customClient, gh.enterprises))
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
				"github_username": {
					DisplayName: "GitHub username",
					Required:    false,
					Description: "The user's GitHub username (optional, used to look up the user if email is private).",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "octocat",
					Order:       3,
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
		l := ctxzap.Extract(ctx)
		_, _, err := gh.customClient.ListEnterpriseConsumedLicenses(ctx, gh.enterprises[0], 1)
		if err != nil {
			l.Debug("baton-github: enterprise features (--enterprises) require a Personal Access Token with enterprise admin scope. "+
				"The consumed-licenses API is not accessible with the current token.",
				zap.Error(err))
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

	if len(gh.enterprises) > 0 {
		l := ctxzap.Extract(ctx)
		_, _, err := gh.customClient.ListEnterpriseConsumedLicenses(ctx, gh.enterprises[0], 1)
		if err != nil {
			l.Debug("baton-github: enterprise features (--enterprises) require a Personal Access Token. "+
				"GitHub App authentication cannot access the consumed-licenses API. "+
				"Either switch to PAT auth or remove the --enterprises flag.",
				zap.Error(err))
		}
	}

	return nil, nil
}

// newGitHubClient returns a new GitHub API client authenticated with an access
// token via oauth2. wrapTransport is an optional hook (may be nil) for stacking
// additional http.RoundTrippers (e.g., ETag conditional-request caching)
// underneath the oauth2 transport.
func newGitHubClient(ctx context.Context, instanceURL string, ts oauth2.TokenSource, wrapTransport func(http.RoundTripper) http.RoundTripper) (*github.Client, error) {
	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}

	if wrapTransport != nil {
		httpClient.Transport = wrapTransport(httpClient.Transport)
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

// etagCacheMaxMBEnv lets operators tune the in-process cache size without
// re-deploying. A 5,000-repo customer with ~100 KB collaborator-list responses
// can easily exceed the default 100 MiB and benefit from raising this.
const etagCacheMaxMBEnv = "BATON_GITHUB_HTTP_CACHE_MAX_MB"

// newETagWrap returns a wrapTransport hook that installs an ETagTransport when
// the opt-in is enabled, or nil to pass through. scope distinguishes cache
// entries between different installations/PATs sharing a process. The returned
// hook appends each constructed transport to the provided sink slice so the
// connector can later report stats on Close.
func newETagWrap(ctx context.Context, enabled bool, scope string, sink *[]*customclient.ETagTransport) func(http.RoundTripper) http.RoundTripper {
	if !enabled {
		return nil
	}
	l := ctxzap.Extract(ctx)
	maxMB := 0
	if v := os.Getenv(etagCacheMaxMBEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxMB = n
		} else {
			l.Warn("baton-github: ignoring invalid "+etagCacheMaxMBEnv,
				zap.String("value", v))
		}
	}
	return func(next http.RoundTripper) http.RoundTripper {
		t, err := customclient.NewETagTransport(next, maxMB, scope)
		if err != nil {
			l.Warn("baton-github: http-etag-cache disabled (init failed)", zap.Error(err))
			return next
		}
		l.Info("baton-github: http-etag-cache enabled", zap.Int("max_mb", maxMB))
		if sink != nil {
			*sink = append(*sink, t)
		}
		return t
	}
}

func newWithGithubPAT(ctx context.Context, ghc *cfg.Github) (*GitHub, error) {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: ghc.Token},
	)
	var etagTransports []*customclient.ETagTransport
	wrap := newETagWrap(ctx, ghc.EnableHttpEtagCache, "pat:"+ghc.Token, &etagTransports)
	ghClient, err := newGitHubClient(ctx, ghc.InstanceUrl, ts, wrap)
	if err != nil {
		return nil, err
	}
	graphqlClient, err := newGitHubGraphqlClient(ctx, ghc.InstanceUrl, ts, wrap)
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
		directCollaboratorsOnly:  ghc.DirectCollaboratorsOnly,
		etagTransports:           etagTransports,
	}, nil
}

func newWithGithubApp(ctx context.Context, ghc *cfg.Github) (*GitHub, error) {
	jwttoken, err := getJWTToken(ghc.AppId, string(ghc.AppPrivatekeyPath))
	if err != nil {
		return nil, err
	}

	// Scope cache entries by app + org so different installations don't share
	// state; the JWT itself rotates every ~9 minutes and would needlessly
	// invalidate the cache if used as the scope.
	var etagTransports []*customclient.ETagTransport
	wrap := newETagWrap(ctx, ghc.EnableHttpEtagCache, "app:"+ghc.AppId+":"+ghc.Org, &etagTransports)

	appClient, err := newGitHubClient(ctx,
		ghc.InstanceUrl,
		oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: jwttoken},
		),
		wrap,
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
		wrap,
	)
	if err != nil {
		return nil, err
	}

	ghClient, err := newGitHubClient(ctx, ghc.InstanceUrl, ts, wrap)
	if err != nil {
		return nil, err
	}
	graphqlClient, err := newGitHubGraphqlClient(ctx, ghc.InstanceUrl, ts, wrap)
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
		etagTransports:           etagTransports,
		graphqlClient:            graphqlClient,
		orgCache:                 newOrgNameCache(ghClient),
		syncSecrets:              ghc.SyncSecrets,
		omitArchivedRepositories: ghc.OmitArchivedRepositories,
		directCollaboratorsOnly:  ghc.DirectCollaboratorsOnly,
	}
	return gh, nil
}

func newGitHubGraphqlClient(ctx context.Context, instanceURL string, ts oauth2.TokenSource, wrapTransport func(http.RoundTripper) http.RoundTripper) (*githubv4.Client, error) {
	instanceURL = strings.TrimSuffix(instanceURL, "/")

	var enterpriseGqlURL string
	if instanceURL != "" && instanceURL != githubDotCom {
		parsed, err := url.Parse(instanceURL)
		if err != nil {
			return nil, err
		}
		parsed.Path = "/api/graphql"
		enterpriseGqlURL = parsed.String()
	}

	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}
	httpClient.Transport = &statusClassifyingTransport{base: httpClient.Transport}
	if wrapTransport != nil {
		httpClient.Transport = wrapTransport(httpClient.Transport)
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	tc := oauth2.NewClient(ctx, ts)

	if enterpriseGqlURL != "" {
		return githubv4.NewEnterpriseClient(enterpriseGqlURL, tc), nil
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
		return nil, wrapGitHubError(err, resp, fmt.Sprintf("github-connector: failed to find installation for org %s", orgName))
	}
	return installation, nil
}

func getInstallationToken(ctx context.Context, c *github.Client, id int64) (*github.InstallationToken, error) {
	l := ctxzap.Extract(ctx)
	token, resp, err := c.Apps.CreateInstallationToken(ctx, id, &github.InstallationTokenOptions{})
	if err != nil {
		l.Warn("failed to create GitHub App installation token",
			zap.Int64("installation_id", id),
			zap.String("github_error", gitHubErrorMessage(err)),
		)
		return nil, fmt.Errorf("github-connector: failed to create installation token for installation %d: %w", id, err)
	}

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		l.Warn("unexpected status creating GitHub App installation token",
			zap.Int64("installation_id", id),
			zap.Int("http_status", resp.StatusCode),
			zap.String("response_body", string(body)),
		)
		return nil, fmt.Errorf("github-connector: unexpected status %d creating installation token for installation %d: %s", resp.StatusCode, id, body)
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
	// Token-mint path is a one-shot POST per refresh — no benefit from ETag caching.
	appClient, err := newGitHubClient(r.ctx,
		r.instanceURL,
		r.jwtTokenSource,
		nil,
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
