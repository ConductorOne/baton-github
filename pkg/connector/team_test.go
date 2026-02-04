package connector

import (
	"context"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	entitlement2 "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/conductorone/baton-github/test"
	"github.com/conductorone/baton-github/test/mocks"
)

func TestTeam(t *testing.T) {
	ctx := context.Background()

	t.Run("should grant and revoke entitlements", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()

		githubOrganization, _, githubTeam, githubUser, _, _ := mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := teamBuilder(githubClient, cache)

		organization, _ := organizationResource(ctx, githubOrganization, nil, false)
		team, _ := teamResource(githubTeam, organization.Id)
		user, _ := userResource(ctx, githubUser, *githubUser.Email, nil)

		entitlement := v2.Entitlement{
			Id:       entitlement2.NewEntitlementID(team, "member"),
			Resource: team,
		}

		grantAnnotations, err := client.Grant(ctx, user, &entitlement)
		require.Nil(t, err)
		require.Empty(t, grantAnnotations)

		_, results, err := client.Grants(ctx, team, resourceSdk.SyncOpAttrs{
			PageToken: pagination.Token{},
			Session:   &noOpSessionStore{},
		})
		require.Nil(t, err)
		test.AssertHasRatelimitAnnotations(t, results.Annotations)
		require.Equal(t, "{\"states\":[{\"type\":\"member\"}],\"current_state\":{\"type\":\"maintainer\"}}", results.NextPageToken)

		grant := v2.Grant{
			Entitlement: &entitlement,
			Principal:   user,
		}

		revokeAnnotations, err := client.Revoke(ctx, &grant)
		require.Nil(t, err)
		require.Empty(t, revokeAnnotations)
	})
}

func TestTeamActions(t *testing.T) {
	ctx := context.Background()

	t.Run("should create a basic team with name, description, notifications, and privacy", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		githubOrganization, _, _, _, _, _ := mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := teamBuilder(githubClient, cache)

		// Create args for the action
		args, err := structpb.NewStruct(map[string]interface{}{
			"name":                  "test-team-basic",
			"description":           "A test team for unit testing",
			"privacy":               "secret",
			"notifications_enabled": true,
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12", // Matches the seeded org ID
			},
		})
		require.NoError(t, err)

		result, annos, err := client.handleCreateTeamAction(ctx, args)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, annos)

		// Verify success field
		successVal := result.Fields["success"]
		require.NotNil(t, successVal)
		require.True(t, successVal.GetBoolValue())

		// Verify resource was returned
		resourceVal := result.Fields["resource"]
		require.NotNil(t, resourceVal)

		_ = githubOrganization // Used in seed
	})

	t.Run("should create a team with multiple maintainers", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		_, _, _, existingUser, _, _ := mgh.Seed()

		// Add a second user
		secondUserID := int64(100)
		secondUserLogin := "100"
		secondUserEmail := "seconduser@example.com"
		mgh.AddUser(github.User{
			ID:    github.Ptr(secondUserID),
			Login: github.Ptr(secondUserLogin),
			Email: github.Ptr(secondUserEmail),
		})

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := teamBuilder(githubClient, cache)

		args, err := structpb.NewStruct(map[string]interface{}{
			"name":        "test-team-maintainers",
			"description": "Team with multiple maintainers",
			"privacy":     "secret",
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12",
			},
			"maintainers": []interface{}{
				map[string]interface{}{
					"resource_type": "user",
					"resource":      "56", // existingUser.ID
				},
				map[string]interface{}{
					"resource_type": "user",
					"resource":      "100", // secondUserID
				},
			},
		})
		require.NoError(t, err)

		result, _, err := client.handleCreateTeamAction(ctx, args)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.Fields["success"].GetBoolValue())

		_ = existingUser // Used in seed
	})

	t.Run("should fail to create nested team when parent team is secret", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		githubOrganization, _, _, _, _, _ := mgh.Seed()

		// Add a secret parent team
		secretParentTeamID := int64(200)
		mgh.AddTeam(github.Team{
			ID:           github.Ptr(secretParentTeamID),
			Name:         github.Ptr("secret-parent-team"),
			Slug:         github.Ptr("secret-parent-team"),
			Organization: githubOrganization,
			Privacy:      github.Ptr("secret"),
		})

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := teamBuilder(githubClient, cache)

		args, err := structpb.NewStruct(map[string]interface{}{
			"name":        "nested-team-under-secret",
			"description": "This should fail because parent is secret",
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12",
			},
			"parent": map[string]interface{}{
				"resource_type": "team",
				"resource":      "200", // Secret parent team ID
			},
		})
		require.NoError(t, err)

		result, _, err := client.handleCreateTeamAction(ctx, args)
		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "cannot create child team")
		require.Contains(t, err.Error(), "secret")
	})

	t.Run("should successfully create nested team when parent team is closed", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		githubOrganization, _, _, _, _, _ := mgh.Seed()

		// Add a closed parent team
		closedParentTeamID := int64(201)
		mgh.AddTeam(github.Team{
			ID:           github.Ptr(closedParentTeamID),
			Name:         github.Ptr("closed-parent-team"),
			Slug:         github.Ptr("closed-parent-team"),
			Organization: githubOrganization,
			Privacy:      github.Ptr("closed"),
		})

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := teamBuilder(githubClient, cache)

		args, err := structpb.NewStruct(map[string]interface{}{
			"name":        "nested-team-under-closed",
			"description": "Nested team under closed parent",
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12",
			},
			"parent": map[string]interface{}{
				"resource_type": "team",
				"resource":      "201", // Closed parent team ID
			},
		})
		require.NoError(t, err)

		result, _, err := client.handleCreateTeamAction(ctx, args)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.Fields["success"].GetBoolValue())
	})

	t.Run("should fail with missing required org field", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := teamBuilder(githubClient, cache)

		// Missing org field
		args, err := structpb.NewStruct(map[string]interface{}{
			"name":        "test-team",
			"description": "A team without org",
		})
		require.NoError(t, err)

		result, _, err := client.handleCreateTeamAction(ctx, args)
		require.Error(t, err)
		require.Nil(t, result)
	})

	t.Run("should fail with invalid privacy value", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := teamBuilder(githubClient, cache)

		args, err := structpb.NewStruct(map[string]interface{}{
			"name":    "test-team-invalid-privacy",
			"privacy": "invalid_privacy_value",
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12",
			},
		})
		require.NoError(t, err)

		result, _, err := client.handleCreateTeamAction(ctx, args)
		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "invalid privacy value")
	})
}
