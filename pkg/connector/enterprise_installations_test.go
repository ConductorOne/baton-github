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

// Without the opt-in the enterprise owner path is left unwired, and this is
// what that has to mean: the same answer the connector gave before the
// capability existed. Reading owners needs the app installed on the enterprise
// account, which no existing deployment has done, and the connector fails the
// sync when it cannot read them — so if the unwired path did anything else,
// upgrading would turn a working sync into a failing one for everyone already
// passing --enterprises under app auth. Under app auth that answer is no
// resources, because the consumed-licenses API it falls back to is PAT-only
// and answers 403.
func TestEnterpriseRoleListIsInertWithoutTheOptIn(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	apiClient := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"}))
	}))

	// nil provider is how newWithGithubApp leaves it when the flag is off.
	builder := EnterpriseRoleBuilder(apiClient, apiClient, customclient.New(apiClient),
		[]string{"example-enterprise"}, nil)

	resources, _, err := builder.List(ctx, nil, resourceSdk.SyncOpAttrs{})
	require.NoError(t, err)
	require.Empty(t, resources)
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
	require.NotContains(t, err.Error(), "personal access token")
}

// The SDK derives CAPABILITY_PROVISION by type-asserting each registered
// syncer, so the split between the two types is the whole mechanism keeping
// provisioning off PAT deployments. Nothing else fails if it is undone: moving
// Grant and Revoke back onto the read-only type, or dropping the check in
// ResourceSyncers, would re-advertise provisioning to PAT with every other
// test still green.
func TestResourceSyncersOfferProvisioningOnlyWhenItWorks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		provider    enterpriseClientProvider
		provisioned bool
	}{
		{"pat or opt-in off", nil, false},
		{"app auth opted in", func(context.Context) (map[string]*githubEnterpriseAdministratorClient, error) {
			return nil, nil
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &GitHub{
				enterprises:              []string{testEnterprise},
				newEnterpriseRoleClients: tc.provider,
			}

			var found bool
			for _, syncer := range gh.ResourceSyncers(context.Background()) {
				if syncer.ResourceType(context.Background()).GetId() != resourceTypeEnterpriseRole.Id {
					continue
				}
				found = true
				_, provisions := syncer.(connectorbuilder.ResourceProvisionerV2Limited)
				require.Equal(t, tc.provisioned, provisions)
			}
			require.True(t, found, "enterprise_role must be synced either way")
		})
	}
}

// Setting the flag in both the environment and the command line lands the
// same enterprise twice, which is still one enterprise. Counting raw entries
// would reject a configuration the operator wrote correctly, with a message
// naming a number they never chose.
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
	require.NotContains(t, err.Error(), "one enterprise at a time")
}

// The owners are read through one organization, which belongs to one
// enterprise, so app auth cannot serve a list and picking one would be a
// guess. This fails for the same reason as a missing installation: a sync that
// completes without owners costs the operator every owner grant.
func TestNewEnterpriseRoleClientsRejectsSeveralEnterprises(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request expected, got %s", r.URL.Path)
	}))

	_, err := newEnterpriseRoleClients(
		ctx,
		ctx,
		"https://github.com",
		client,
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "unused"}),
		[]string{"one-enterprise", "another-enterprise"},
		nil,
		"example-org",
	)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "one enterprise at a time")
}
