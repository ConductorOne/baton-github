package connector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/conductorone/baton-github/test"
	"github.com/conductorone/baton-github/test/mocks"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/require"
)

func TestUsersList(t *testing.T) {
	ctx := context.Background()

	t.Run("should get a list of users", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()

		githubOrganization, _, _, githubUser, _, _ := mgh.Seed()

		organization, err := organizationResource(
			ctx,
			githubOrganization,
			nil,
			false,
		)
		if err != nil {
			t.Error(err)
		}

		githubClient := github.NewClient(mgh.Server())
		graphQLClient := mocks.MockGraphQL()
		cache := newOrgNameCache(githubClient)
		client := UserBuilder(
			githubClient,
			graphQLClient,
			cache,
			[]string{organization.DisplayName},
			nil,
			nil,
		)

		users, results, err := client.List(
			ctx,
			organization.Id,
			resourceSdk.SyncOpAttrs{
				PageToken: pagination.Token{},
				Session:   &noOpSessionStore{},
			},
		)
		require.Nil(t, err)
		test.AssertHasRatelimitAnnotations(t, results.Annotations)
		require.Equal(t, "", results.NextPageToken)
		require.Len(t, users, 1)
		require.Equal(t, *githubUser.Login, users[0].Id.Resource)
	})
}

func mockGraphQLWithoutSAML(t *testing.T, verifiedDomainEmails []string) *githubv4.Client {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		var payload map[string]any
		switch {
		case strings.Contains(string(body), "organizationVerifiedDomainEmails"):
			payload = map[string]any{"data": map[string]any{
				"user":      map[string]any{"organizationVerifiedDomainEmails": verifiedDomainEmails},
				"rateLimit": map[string]any{"limit": 5000, "cost": 1, "remaining": 4999, "resetAt": "2030-01-01T00:00:00Z"},
			}}
		default:
			payload = map[string]any{"data": map[string]any{
				"organization": map[string]any{"samlIdentityProvider": map[string]any{"id": "", "ssoUrl": ""}},
			}}
		}
		require.NoError(t, json.NewEncoder(w).Encode(payload))
	}))
	t.Cleanup(server.Close)
	return githubv4.NewEnterpriseClient(server.URL, server.Client())
}

func TestUsersListVerifiedDomainEmailFallback(t *testing.T) {
	ctx := context.Background()

	listUsers := func(t *testing.T, verifiedDomainEmails []string) *v2.Resource {
		mgh := mocks.NewMockGitHub()
		githubOrganization, _, _, githubUser, _, _ := mgh.Seed()
		githubUser.Email = nil
		mgh.SetUser(*githubUser)

		organization, err := organizationResource(ctx, githubOrganization, nil, false)
		require.NoError(t, err)

		githubClient := github.NewClient(mgh.Server())
		client := UserBuilder(
			githubClient,
			mockGraphQLWithoutSAML(t, verifiedDomainEmails),
			newOrgNameCache(githubClient),
			[]string{organization.DisplayName},
			nil,
			nil,
		)

		users, _, err := client.List(ctx, organization.Id, resourceSdk.SyncOpAttrs{
			PageToken: pagination.Token{},
			Session:   &noOpSessionStore{},
		})
		require.NoError(t, err)
		require.Len(t, users, 1)
		return users[0]
	}

	t.Run("uses the first verified-domain email as primary and keeps the rest", func(t *testing.T) {
		user := listUsers(t, []string{"not-an-email", "alice@verified.example", "alice@other.example"})

		trait, err := resourceSdk.GetUserTrait(user)
		require.NoError(t, err)
		require.Len(t, trait.Emails, 2)
		require.Equal(t, "alice@verified.example", trait.Emails[0].Address)
		require.True(t, trait.Emails[0].IsPrimary)
		require.Equal(t, "alice@other.example", trait.Emails[1].Address)
		require.False(t, trait.Emails[1].IsPrimary)
	})

	t.Run("leaves the email empty when the org has no verified-domain email for the member", func(t *testing.T) {
		user := listUsers(t, []string{})

		trait, err := resourceSdk.GetUserTrait(user)
		require.NoError(t, err)
		for _, email := range trait.Emails {
			require.Empty(t, email.Address)
		}
	})
}
