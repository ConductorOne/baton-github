package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	cfg "github.com/conductorone/baton-github/pkg/config"
	"github.com/conductorone/baton-github/pkg/customclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// GitHub answers 404 when the app is not installed on the enterprise. That
// enterprise is skipped instead of erroring: an error out of the build reaches
// List, and the SDK aborts the entire sync on anything but NotFound, so a
// configuration that synced users and orgs fine would stop syncing at all.
// Skipping leaves no client, which is what makes Grant and Revoke report it.
func TestNewEnterpriseRoleClientsSkipsAnEnterpriseWithoutAnInstall(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/enterprises/example-enterprise/installation", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"}))
	}))

	clients, err := newEnterpriseRoleClients(
		ctx,
		ctx,
		"https://github.com",
		client,
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "unused"}),
		[]string{"example-enterprise"},
		nil,
		"example-org",
	)
	require.NoError(t, err)
	require.Empty(t, clients)
}

// The owners are read through one organization, which belongs to one
// enterprise, so app auth cannot serve a list. Rejecting it while the
// connector is being built keeps it out of the sync, where the SDK would turn
// it into an aborted run instead of a message about the configuration.
func TestNewWithGithubAppRejectsSeveralEnterprises(t *testing.T) {
	t.Parallel()

	_, err := newWithGithubApp(context.Background(), &cfg.Github{
		Org:         "example-org",
		Enterprises: []string{"one-enterprise", "another-enterprise"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "one enterprise at a time")
}

// An error out of List aborts the whole sync: the SDK downgrades only
// NotFound, and anything else cancels the batch. So an app that is not
// installed on the enterprise has to leave List reporting what it did before
// enterprise installations were read at all, which under app auth is nothing:
// the consumed-licenses API it falls back to is PAT-only and answers 403.
func TestEnterpriseRoleListSurvivesWithoutEnterpriseClients(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	apiClient := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"}))
	}))

	builder := EnterpriseRoleBuilder(apiClient, apiClient, customclient.New(apiClient), []string{"example-enterprise"},
		func(context.Context) (map[string]*githubEnterpriseAdministratorClient, error) {
			return map[string]*githubEnterpriseAdministratorClient{}, nil
		},
	)

	resources, _, err := builder.List(ctx, nil, resourceSdk.SyncOpAttrs{})
	require.NoError(t, err)
	require.Empty(t, resources)
}
