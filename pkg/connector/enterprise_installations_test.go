package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
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

func TestListEnterpriseInstallations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	payload := []map[string]any{
		{
			"id":          int64(11),
			"target_type": "Organization",
			"account":     map[string]any{"login": "example-org"},
		},
		{
			"id":          int64(22),
			"target_type": "Enterprise",
			"account":     map[string]any{"slug": "Example-Enterprise"},
		},
		{
			"id":          int64(33),
			"target_type": "Enterprise",
			"account":     map[string]any{"login": "missing-slug-enterprise"},
		},
	}

	client := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/app/installations", r.URL.Path)
		require.Equal(t, "1", r.URL.Query().Get("page"))
		require.Equal(t, "100", r.URL.Query().Get("per_page"))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(payload))
	}))

	installations, err := listEnterpriseInstallations(ctx, customclient.New(client))
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"example-enterprise": 22}, installations)
}

// On GitHub Enterprise Server, WithEnterpriseURLs puts the REST API under
// /api/v3. Asking api.github.com instead would report the app as uninstalled
// on an enterprise that does have it.
func TestListEnterpriseInstallationsUsesTheInstanceBaseURL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newGitHubAPITestClientAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/app/installations", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode([]map[string]any{
			{
				"id":          int64(22),
				"target_type": "Enterprise",
				"account":     map[string]any{"slug": "ghes-enterprise"},
			},
		}))
	}), "/api/v3/")

	installations, err := listEnterpriseInstallations(ctx, customclient.New(client))
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"ghes-enterprise": 22}, installations)
}

// The endpoint reports no total, so a page shorter than the requested size is
// what ends the walk.
func TestListEnterpriseInstallationsPagination(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var requests atomic.Int32

	client := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/app/installations", r.URL.Path)
		requests.Add(1)

		page := r.URL.Query().Get("page")
		installations := make([]map[string]any, 0, customclient.AppInstallationsPageSize)
		switch page {
		case "1":
			// A full page: one enterprise plus filler, so the caller must ask
			// for the next one.
			installations = append(installations, map[string]any{
				"id":          int64(22),
				"target_type": "Enterprise",
				"account":     map[string]any{"slug": "first-enterprise"},
			})
			for i := 1; i < customclient.AppInstallationsPageSize; i++ {
				installations = append(installations, map[string]any{
					"id":          int64(1000 + i),
					"target_type": "Organization",
					"account":     map[string]any{"login": fmt.Sprintf("org-%d", i)},
				})
			}
		case "2":
			installations = append(installations, map[string]any{
				"id":          int64(44),
				"target_type": "Enterprise",
				"account":     map[string]any{"slug": "second-enterprise"},
			})
		default:
			t.Fatalf("unexpected page %q: the walk must stop on a short page", page)
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(installations))
	}))

	installations, err := listEnterpriseInstallations(ctx, customclient.New(client))
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"first-enterprise": 22, "second-enterprise": 44}, installations)
	require.Equal(t, int32(2), requests.Load())
}

func TestNewEnterpriseRoleClientsRequiresEnterpriseInstall(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newGitHubAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/app/installations", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode([]map[string]any{
			{
				"id":          int64(11),
				"target_type": "Organization",
				"account":     map[string]any{"login": "example-org"},
			},
		}))
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
