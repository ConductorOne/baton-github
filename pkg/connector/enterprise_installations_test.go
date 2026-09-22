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

	"github.com/conductorone/baton-github/pkg/customclient"
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

// GitHub answers 404 when the app is not installed on the enterprise. That has
// to fail this resource type with an actionable message rather than leave the
// connector reporting an empty owner set, which C1 reads as a revoke.
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
	require.Error(t, err)
	require.Contains(t, err.Error(), `not installed on enterprise "example-enterprise"`)
	require.NotContains(t, err.Error(), "personal access token")
}
