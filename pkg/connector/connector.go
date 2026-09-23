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
		// Invitations emit TRAIT_USER with STATUS_PENDING.
		// Accepted members from user.go emit STATUS_ENABLED.
		Traits: []v2.ResourceType_Trait{
			v2.ResourceType_TRAIT_USER,
		},
		// Invitations disappear once accepted, so their count can legitimately
		// drop between syncs. Skip sync anomaly detection for this type only.
		Annotations: append(v1AnnotationsForResourceType("invitation"), annotations.New(&v2.SkipSyncAnomalyDetection{})...),
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
	resourceTypeLicense = &v2.ResourceType{
		Id:          "license",
		DisplayName: "License",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_LICENSE_PROFILE},
		Annotations: annotations.New(
			&v2.V1Identifier{
				Id: "license",
			},
			&v2.SkipEntitlements{},
			&v2.OptInRequired{},
		),
	}
	resourceTypeApp = &v2.ResourceType{
		Id:          "app",
		DisplayName: "GitHub App",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_APP},
		Annotations: skipEntitlementsAndGrantsAnnotations("app", "organization_administration:read"),
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
	newEnterpriseRoleClients enterpriseClientProvider
	syncLastActivity         bool
}

func (gh *GitHub) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncerV2 {
	resourceSyncers := []connectorbuilder.ResourceSyncerV2{
		OrgBuilder(gh.client, gh.appClient, gh.orgCache, gh.orgs, gh.syncSecrets),
		TeamBuilder(gh.client, gh.orgCache, gh.directCollaboratorsOnly),
		UserBuilder(gh.client, gh.graphqlClient, gh.orgCache, gh.orgs, gh.customClient, gh.enterprises),
		RepositoryBuilder(gh.client, gh.orgCache, gh.omitArchivedRepositories, gh.directCollaboratorsOnly),
		OrgRoleBuilder(gh.client, gh.orgCache),
		InvitationBuilder(InvitationBuilderParams{
			client:   gh.client,
			orgCache: gh.orgCache,
			orgs:     gh.orgs,
		}),
		AppBuilder(gh.client, gh.orgCache),
	}

	if gh.syncSecrets {
		resourceSyncers = append(resourceSyncers, APITokenBuilder(gh.client, gh.orgCache))
	}

	if gh.syncLastActivity {
		// usageAppBuilder only exists to support usageEventFeed, so it's gated the same way.
		resourceSyncers = append(resourceSyncers, newUsageAppBuilder())
	}

	if len(gh.enterprises) > 0 {
		// The provisioning-capable syncer is registered only where the role can
		// actually be provisioned. The SDK reads CAPABILITY_PROVISION off the
		// methods a syncer implements, so registering it everywhere would offer
		// the role as requestable to PAT deployments, where every request fails.
		if gh.newEnterpriseRoleClients != nil {
			resourceSyncers = append(resourceSyncers, EnterpriseRoleProvisioningBuilder(
				gh.client, gh.appClient, gh.customClient, gh.enterprises,
				gh.newEnterpriseRoleClients,
			))
		} else {
			resourceSyncers = append(resourceSyncers, EnterpriseRoleBuilder(
				gh.client, gh.appClient, gh.customClient, gh.enterprises, nil,
			))
		}
		// The consumed-licenses API behind this type is PAT-only and its 403
		// fails the whole sync, so under app auth it has never emitted a
		// resource and can only break the run. Not registering it deletes
		// nothing, and spares the operator having to disable the type in C1
		// to make the documented enterprise setup sync at all.
		if gh.appClient == nil {
			resourceSyncers = append(resourceSyncers, LicenseBuilder(gh.customClient, gh.enterprises))
		}
	}
	return resourceSyncers
}

func (gh *GitHub) EventFeeds(_ context.Context) []connectorbuilder.EventFeed {
	if !gh.syncLastActivity {
		return nil
	}

	return []connectorbuilder.EventFeed{
		newUsageEventFeed(gh.client, gh.orgs),
	}
}

// Metadata returns metadata about the connector.
func (gh *GitHub) Metadata(_ context.Context) (*v2.ConnectorMetadata, error) {
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
			l.Debug("baton-github: enterprise license data requires a Personal Access Token. "+
				"GitHub App authentication cannot access the consumed-licenses API, "+
				"so the license resource type cannot sync.",
				zap.Error(err))
		}
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
		directCollaboratorsOnly:  ghc.DirectCollaboratorsOnly,
		syncLastActivity:         ghc.SyncLastActivity,
	}, nil
}

// appPrivateKeyPEM returns the GitHub App private key PEM contents to use,
// preferring the in-memory app-privatekey flag over the on-disk
// app-privatekey-path. Providing either one satisfies the requirement; if
// neither is set an error is returned.
func appPrivateKeyPEM(ghc *cfg.Github) (string, error) {
	if ghc.AppPrivatekey != "" {
		return ghc.AppPrivatekey, nil
	}
	if len(ghc.AppPrivatekeyPath) > 0 {
		return string(ghc.AppPrivatekeyPath), nil
	}
	return "", errors.New("github app authentication requires either --app-privatekey or --app-privatekey-path")
}

func newWithGithubApp(ctx context.Context, ghc *cfg.Github) (*GitHub, error) {
	privateKey, err := appPrivateKeyPEM(ghc)
	if err != nil {
		return nil, err
	}

	jwttoken, err := getJWTToken(ghc.AppId, privateKey)
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
			privateKey: privateKey,
		},
	)
	// Wrap the installation-token refresher in a refreshableTokenSource so the
	// 401-retry middleware can mark the cached token dead when GitHub rotates
	// it server-side before its stated Expiry.
	ts := newRefreshableTokenSource(
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

	// Build the layered HTTP client for the installation-token path. The
	// tokenRefreshTransport observes 401s, invalidates ts, and retries once
	// with the freshly-issued installation token.
	appHTTPClient, err := newGitHubAppHTTPClient(ctx, ts)
	if err != nil {
		return nil, err
	}
	ghClient, graphqlClient, err := newGitHubAppClients(ghc.InstanceUrl, appHTTPClient)
	if err != nil {
		return nil, err
	}

	// Enterprise administration needs its own installation token: the org
	// installation token above carries no enterprise permissions. Reading the
	// owners needs the org token, so both are handed to the client.
	//
	// Built on first use rather than here, so connector construction and
	// Validate do not depend on the enterprise installation and a later sync
	// retries the build. It does not narrow the blast radius of a failure:
	// the error surfaces from List, which fails the whole sync, and that is
	// deliberate — a sync that completed without owners would read to C1 as a
	// revoke of every owner assignment.
	//
	// The construction context is captured separately: the clients are
	// memoized for the process lifetime, so the token refresher inside them
	// must not hold the context of whichever RPC happened to build them.
	connectorCtx := ctx
	// Left nil unless the operator opted in. Reading enterprise owners needs
	// the app installed on the enterprise account too, which no existing
	// deployment has done, and the connector fails the sync when it cannot
	// read them. Nil keeps that path inert: enterprise roles then come from
	// the consumed-licenses cache exactly as they did before this capability,
	// which under app auth means nothing, so an upgrade changes no behaviour
	// until it is asked for.
	var newEnterpriseRoleClientsFn enterpriseClientProvider
	if ghc.EnableEnterpriseOwnerProvisioning {
		newEnterpriseRoleClientsFn = func(ctx context.Context) (map[string]*githubEnterpriseAdministratorClient, error) {
			return newEnterpriseRoleClients(
				ctx, connectorCtx, ghc.InstanceUrl, appClient, jwtts, ghc.Enterprises, appHTTPClient, ghc.Org)
		}
	}

	gh := &GitHub{
		client:                   ghClient,
		appClient:                appClient,
		customClient:             customclient.New(ghClient),
		instanceURL:              ghc.InstanceUrl,
		orgs:                     []string{ghc.Org},
		enterprises:              ghc.Enterprises,
		newEnterpriseRoleClients: newEnterpriseRoleClientsFn,
		graphqlClient:            graphqlClient,
		orgCache:                 newOrgNameCache(ghClient),
		syncSecrets:              ghc.SyncSecrets,
		omitArchivedRepositories: ghc.OmitArchivedRepositories,
		directCollaboratorsOnly:  ghc.DirectCollaboratorsOnly,
		syncLastActivity:         ghc.SyncLastActivity,
	}
	return gh, nil
}

// newEnterpriseRoleClients builds one client per configured enterprise, each
// with that enterprise's own installation token: enterprise and organization
// installations are separate, and an enterprise mutation rejects the org token.
//
// Fails closed when the app is not installed on an enterprise, or when the
// organization does not belong to it. Returning no clients instead would let
// the sync complete while reading no owners, and C1 deletes every resource of
// a type that a completed sync did not report — the Owner role and every
// grant on it. Failing is what stops a sync from being completed at all, so
// an operator who has not installed the app on the enterprise account, or who
// configured several, hears about it instead of losing the assignments.
//
// ctx scopes the discovery requests to the caller. connectorCtx outlives them
// and is what the memoized clients keep for refreshing the installation token,
// which expires after an hour or on the first 401.
func newEnterpriseRoleClients(
	ctx context.Context,
	connectorCtx context.Context,
	instanceURL string,
	appClient *github.Client,
	jwtTokenSource oauth2.TokenSource,
	enterprises []string,
	orgHTTPClient *http.Client,
	org string,
) (map[string]*githubEnterpriseAdministratorClient, error) {
	if len(enterprises) == 0 {
		return nil, nil
	}
	// The owners are read through the single configured organization, and an
	// organization belongs to exactly one enterprise, so app auth cannot serve
	// a list and picking one of them would be a guess. The PAT path does
	// support a list.
	if len(enterprises) > 1 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"github-connector: GitHub App authentication serves one enterprise at a time, "+
				"because the owners are read through organization %q, which belongs to a single enterprise; "+
				"%d were configured", org, len(enterprises))
	}

	// NewBaseHttpClient reports a failed cache setup by returning nil, which
	// only panics later inside Do.
	installationClient := customclient.New(appClient)
	if installationClient.BaseHttpClient == nil {
		return nil, fmt.Errorf("github-connector: error building the enterprise installation client")
	}

	clients := make(map[string]*githubEnterpriseAdministratorClient, len(enterprises))
	for _, enterprise := range enterprises {
		installation, _, err := installationClient.GetEnterpriseInstallation(ctx, enterprise)
		if err != nil {
			// A 404 is the app not being installed on the enterprise, which is
			// the misconfiguration worth naming.
			if status.Code(err) == codes.NotFound {
				return nil, status.Errorf(codes.FailedPrecondition,
					"github-connector: GitHub App is not installed on enterprise %q; install it on the enterprise account "+
						"with the Enterprise people read and write permission", enterprise)
			}
			return nil, err
		}
		installationID := installation.ID

		token, err := getInstallationToken(ctx, appClient, installationID)
		if err != nil {
			return nil, err
		}

		ts := newRefreshableTokenSource(
			&oauth2.Token{
				AccessToken: token.GetToken(),
				Expiry:      token.GetExpiresAt().Time,
			},
			&appTokenRefresher{
				ctx:            connectorCtx,
				instanceURL:    instanceURL,
				installationID: installationID,
				jwtTokenSource: jwtTokenSource,
			},
		)

		httpClient, err := newGitHubAppHTTPClient(connectorCtx, ts)
		if err != nil {
			return nil, err
		}

		client, err := newEnterpriseAdministratorClient(instanceURL, httpClient, orgHTTPClient, org)
		if err != nil {
			return nil, err
		}
		if err := client.verifyOrganization(ctx, enterprise); err != nil {
			return nil, err
		}
		if err := client.resolveEnterpriseNodeID(ctx, enterprise); err != nil {
			return nil, err
		}
		clients[enterprise] = client
	}

	return clients, nil
}

func newGitHubGraphqlClient(ctx context.Context, instanceURL string, ts oauth2.TokenSource) (*githubv4.Client, error) {
	endpoint, err := enterpriseGraphQLEndpoint(instanceURL)
	if err != nil {
		return nil, err
	}

	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}
	httpClient.Transport = &statusClassifyingTransport{base: httpClient.Transport}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	tc := oauth2.NewClient(ctx, ts)

	return githubv4.NewEnterpriseClient(endpoint.String(), tc), nil
}

// escapedLineBreaks unescapes LF-, CRLF-, and CR-escaped line breaks (`\r\n`,
// `\n`, `\r`) to a real newline.
var escapedLineBreaks = strings.NewReplacer(`\r\n`, "\n", `\n`, "\n", `\r`, "\n")

func loadPrivateKeyFromString(p string) (*rsa.PrivateKey, error) {
	p = escapedLineBreaks.Replace(p)
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
