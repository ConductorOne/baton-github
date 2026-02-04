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

func TestRepository(t *testing.T) {
	ctx := context.Background()

	t.Run("should grant and revoke entitlements", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()

		githubOrganization, githubRepository, _, githubUser, _, _ := mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := repositoryBuilder(githubClient, cache, false)

		organization, _ := organizationResource(ctx, githubOrganization, nil, false)
		repository, _ := repositoryResource(ctx, githubRepository, organization.Id)
		user, _ := userResource(ctx, githubUser, *githubUser.Email, nil)

		entitlement := v2.Entitlement{
			Id:       entitlement2.NewEntitlementID(repository, "admin"),
			Resource: repository,
		}

		grantAnnotations, err := client.Grant(ctx, user, &entitlement)
		require.Nil(t, err)
		require.Empty(t, grantAnnotations)

		grants := make([]*v2.Grant, 0)
		bag := &pagination.Bag{}
		for {
			pToken := pagination.Token{}
			state := bag.Current()
			if state != nil {
				token, _ := bag.Marshal()
				pToken.Token = token
			}

			nextGrants, results, err := client.Grants(ctx, repository, resourceSdk.SyncOpAttrs{
				PageToken: pToken,
				Session:   &noOpSessionStore{},
			})
			grants = append(grants, nextGrants...)

			require.Nil(t, err)
			test.AssertHasRatelimitAnnotations(t, results.Annotations)
			if results.NextPageToken == "" {
				break
			}

			err = bag.Unmarshal(results.NextPageToken)
			if err != nil {
				t.Error(err)
			}
		}

		require.Len(t, grants, 1)

		grant := v2.Grant{
			Entitlement: &entitlement,
			Principal:   user,
		}

		revokeAnnotations, err := client.Revoke(ctx, &grant)
		require.Nil(t, err)
		require.Empty(t, revokeAnnotations)
	})
}

func TestRepositoryActions(t *testing.T) {
	ctx := context.Background()

	t.Run("should create a basic repository with name, description and optional fields", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		githubOrganization, _, _, _, _, _ := mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := repositoryBuilder(githubClient, cache, false)

		// Create args for the action
		args, err := structpb.NewStruct(map[string]interface{}{
			"name":               "test-repo-basic",
			"description":        "A test repository for unit testing",
			"visibility":         "private",
			"add_readme":         true,
			"gitignore_template": "Node",
			"license_template":   "apache-2.0",
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12", // Matches the seeded org ID
			},
		})
		require.NoError(t, err)

		result, annos, err := client.handleCreateRepositoryAction(ctx, args)
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

	t.Run("should create a public repository", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := repositoryBuilder(githubClient, cache, false)

		args, err := structpb.NewStruct(map[string]interface{}{
			"name":        "test-repo-public",
			"description": "A public test repository",
			"visibility":  "public",
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12",
			},
		})
		require.NoError(t, err)

		result, _, err := client.handleCreateRepositoryAction(ctx, args)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.Fields["success"].GetBoolValue())
	})

	t.Run("should fail when templates are used but add_readme is false", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := repositoryBuilder(githubClient, cache, false)

		args, err := structpb.NewStruct(map[string]interface{}{
			"name":               "test-repo-template-no-readme",
			"description":        "This should fail",
			"visibility":         "private",
			"add_readme":         false, // Explicitly false with templates
			"gitignore_template": "Python",
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12",
			},
		})
		require.NoError(t, err)

		result, _, err := client.handleCreateRepositoryAction(ctx, args)
		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "add_readme must be true")
	})

	t.Run("should fail with missing required name field", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := repositoryBuilder(githubClient, cache, false)

		// Missing name field
		args, err := structpb.NewStruct(map[string]interface{}{
			"description": "A repo without a name",
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12",
			},
		})
		require.NoError(t, err)

		result, _, err := client.handleCreateRepositoryAction(ctx, args)
		require.Error(t, err)
		require.Nil(t, result)
	})

	t.Run("should fail with invalid visibility value", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := repositoryBuilder(githubClient, cache, false)

		args, err := structpb.NewStruct(map[string]interface{}{
			"name":       "test-repo-invalid-visibility",
			"visibility": "invalid_visibility_value",
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12",
			},
		})
		require.NoError(t, err)

		result, _, err := client.handleCreateRepositoryAction(ctx, args)
		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "invalid visibility")
	})

	t.Run("should create internal repository (enterprise feature)", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()
		mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := repositoryBuilder(githubClient, cache, false)

		args, err := structpb.NewStruct(map[string]interface{}{
			"name":        "test-repo-internal",
			"description": "An internal repository",
			"visibility":  "internal",
			"org": map[string]interface{}{
				"resource_type": "org",
				"resource":      "12",
			},
		})
		require.NoError(t, err)

		result, _, err := client.handleCreateRepositoryAction(ctx, args)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.Fields["success"].GetBoolValue())
	})
}
