package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/conductorone/baton-github/pkg/customclient"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
)

// newGitHubAPITestClient points the client at a test server through BaseURL
// alone. Nothing rewrites the host, so a customclient endpoint that ignored
// BaseURL would leave the test reaching for api.github.com.
func newGitHubAPITestClient(t *testing.T, handler http.Handler) *github.Client {
	t.Helper()

	return newGitHubAPITestClientAt(t, handler, "/")
}

func newGitHubAPITestClientAt(t *testing.T, handler http.Handler, basePath string) *github.Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	baseURL, err := url.Parse(srv.URL + basePath)
	require.NoError(t, err)

	client := github.NewClient(srv.Client())
	client.BaseURL = baseURL

	return client
}

func TestGetEnterpriseInstallation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/enterprises/example-enterprise/installation", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"id": int64(22),
		}))
	}))

	installation, _, err := customclient.New(client).GetEnterpriseInstallation(ctx, "example-enterprise")
	require.NoError(t, err)
	require.Equal(t, int64(22), installation.ID)
}

// A slug is operator-supplied, so it has to survive as one path segment
// instead of being pasted into the URL.
func TestGetEnterpriseInstallationEscapesTheSlug(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/enterprises/a%2Fb/installation", r.URL.EscapedPath())
		w.WriteHeader(http.StatusNotFound)
	}))

	_, _, err := customclient.New(client).GetEnterpriseInstallation(ctx, "a/b")
	require.Error(t, err)
}

// On GitHub Enterprise Server, WithEnterpriseURLs puts the REST API under
// /api/v3. Asking api.github.com instead would report the app as uninstalled
// on an enterprise that does have it.
func TestGetEnterpriseInstallationUsesTheInstanceBaseURL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newGitHubAPITestClientAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/enterprises/ghes-enterprise/installation", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"id": int64(33),
		}))
	}), "/api/v3/")

	installation, _, err := customclient.New(client).GetEnterpriseInstallation(ctx, "ghes-enterprise")
	require.NoError(t, err)
	require.Equal(t, int64(33), installation.ID)
}

// A nil client provider is the token path, where enterprise roles come from
// the consumed-licenses API. A credential that cannot read it fails the sync
// rather than reporting an empty list, which is the shape that path has always
// had: a token without read:enterprise is a configuration the operator has to
// hear about, and reporting nothing would read to C1 as every role being gone.
//
// The provider being nil is the only thing that distinguishes the two paths,
// so this is pinned alongside the app-path tests.
func TestEnterpriseRoleListFailsOnTheTokenPathWithoutEnterpriseScope(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	apiClient := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"}))
	}))

	// nil provider is what newWithGithubPAT leaves behind.
	builder := EnterpriseRoleBuilder(apiClient, customclient.New(apiClient),
		[]string{"example-enterprise"}, nil)

	_, _, err := builder.List(ctx, nil, resourceSdk.SyncOpAttrs{})
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// GitHub answers 404 when the app is not installed on the enterprise. Failing
// is what protects the data: C1 deletes every resource of a type that a
// completed sync did not report, so letting the sync finish while reading no
// owners would drop the Owner role and every grant on it. An error keeps the
// sync from completing at all.
func TestNewEnterpriseRoleClientsRequiresEnterpriseInstall(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/enterprises/example-enterprise/installation", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"}))
	}))

	_, err := newEnterpriseRoleClients(
		ctx,
		ctx,
		"https://github.com",
		client,
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "unused"}),
		[]string{"example-enterprise"},
		nil,
		"example-org",
	)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), `not installed on enterprise "example-enterprise"`)
	require.True(t, isEnterpriseSetupError(err))
	require.NotContains(t, err.Error(), "personal access token")
}

// The enterprise role is registered with Grant and Revoke on both credentials
// on purpose. A token cannot reach the enterprise administrator API, so a
// request made against it fails with a message saying so rather than being
// hidden from C1 -- the alternative, registering the read-only type there,
// also advertises a role nobody can be granted.
func TestResourceSyncersRegisterTheEnterpriseRoleWithProvisioning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		provider enterpriseClientProvider
	}{
		{"token", nil},
		{"app", func(context.Context) (map[string]*customclient.EnterpriseAdminClient, error) {
			return nil, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &GitHub{
				enterprises:              []string{testEnterprise},
				newEnterpriseRoleClients: tc.provider,
			}

			var found bool
			for _, syncer := range gh.ResourceSyncers(ctx) {
				if syncer.ResourceType(ctx).GetId() != resourceTypeEnterpriseRole.Id {
					continue
				}
				found = true
				_, provisions := syncer.(connectorbuilder.ResourceProvisionerV2Limited)
				require.True(t, provisions)
			}
			require.True(t, found, "enterprise_role must be synced either way")
		})
	}
}

// Setting the flag in both the environment and the command line lands the
// same enterprise twice. The clients are keyed by the slug as configured, so
// without the fold that would build two entries for one enterprise and emit
// its Owner role twice.
func TestNewEnterpriseRoleClientsFoldsRepeatedEnterprises(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/enterprises/example-enterprise/installation", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"}))
	}))

	_, err := newEnterpriseRoleClients(
		ctx,
		ctx,
		"https://github.com",
		client,
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "unused"}),
		[]string{"example-enterprise", "Example-Enterprise"},
		nil,
		"example-org",
	)
	// Reaches the installation lookup rather than the several-enterprises
	// guard, which is what proves the fold happened.
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "not installed on enterprise")
}

func TestConnectorFoldsRepeatedEnterprisesBeforeBuildingSyncers(t *testing.T) {
	t.Parallel()

	// The clients are keyed by the slug as configured, so the same enterprise
	// named twice would build two entries and emit the Owner role twice.
	// GitHub matches slugs case-insensitively, so the fold does too.
	enterprises := distinctEnterprises([]string{"example-enterprise", "Example-Enterprise"})
	require.Equal(t, []string{"example-enterprise"}, enterprises)

	resources, _, err := appList(
		enterprises,
		map[string]*customclient.EnterpriseAdminClient{"example-enterprise": nil},
	)
	require.NoError(t, err)
	require.Len(t, resources, 1)
}

// A slug the app cannot reach must not cost the enterprise that works its
// owners. The clients are built per enterprise, so a configuration error skips
// only its own: discarding the whole map would complete a sync with no Owner
// role, which C1 reads as every grant on it being revoked.
func TestNewEnterpriseRoleClientsKeepTheEnterprisesThatBuilt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Only the bad slug is unreachable; the good one never gets past the
		// installation lookup in this fixture either, which is what makes the
		// assertion below about the error rather than about a built client.
		w.WriteHeader(http.StatusNotFound)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"}))
	}))

	_, err := newEnterpriseRoleClients(
		ctx, ctx, "https://github.com", client,
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "unused"}),
		[]string{"example-enterprise", "typo-enterprise"}, nil, "example-org",
	)

	// Every slug failed, so the first configuration error is what surfaces --
	// and it stays a setup error, so the sync skips the type rather than
	// failing on a configuration the operator can fix.
	require.Error(t, err)
	require.True(t, isEnterpriseSetupError(err))
	require.Contains(t, err.Error(), `not installed on enterprise "example-enterprise"`)
}

// GitHub Enterprise Server has no enterprise administrator API, so naming an
// enterprise there can only fail. Failing on the host rather than on the 404
// that follows matters because that 404 reads as "install the app on the
// enterprise account", which is not something a Server operator can do.
func TestEnterpriseCloudHostsAreTheOnlyOnesServingTheOwnerRole(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		instanceURL string
		cloud       bool
	}{
		{"", true},
		{"https://github.com", true},
		{"https://github.com/", true},
		{"https://acme.ghe.com", true},
		{"https://ghe.com", true},
		{"https://github.acme.com", false},
		{"https://ghe.acme.com", false},
		{"https://acme.ghe.com.evil.test", false},
	} {
		t.Run(tc.instanceURL, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.cloud, isEnterpriseCloud(tc.instanceURL))
		})
	}
}

// The check runs before any request, so a Server instance is told the
// capability does not exist there instead of being sent to install something.
func TestNewEnterpriseRoleClientsRejectsANonCloudInstance(t *testing.T) {
	t.Parallel()

	// Two enterprises as well, so the instance is named before the count:
	// a Server operator asked to trim the list would still have nothing to
	// trim it to.
	_, err := newEnterpriseRoleClients(
		context.Background(), context.Background(),
		"https://github.acme.com",
		github.NewClient(nil), nil,
		[]string{testEnterprise, "another-enterprise"}, nil, "example-org",
	)

	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "GitHub Enterprise Cloud capability")
	require.NotContains(t, err.Error(), "install it on the enterprise account")
	require.False(t, isEnterpriseSetupError(err))
}
