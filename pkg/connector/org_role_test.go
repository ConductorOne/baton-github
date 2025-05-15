package connector

import (
	"context"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	entitlement2 "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/google/go-github/v63/github"
	"github.com/stretchr/testify/require"

	"github.com/conductorone/baton-github/test"
	"github.com/conductorone/baton-github/test/mocks"
)

func TestOrgRole(t *testing.T) {
	ctx := context.Background()

	t.Run("should grant and revoke entitlements", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()

		githubOrganization, _, _, githubUser, _ := mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := orgRoleBuilder(githubClient, cache)

		organization, _ := organizationResource(ctx, githubOrganization, nil)
		role, _ := orgRoleResource(ctx, &OrganizationRole{
			ID:          1,
			Name:        "Test Role",
			Description: "Test Role Description",
		}, organization)
		user, _ := userResource(ctx, githubUser, *githubUser.Email, nil)

		entitlement := v2.Entitlement{
			Id:       entitlement2.NewEntitlementID(role, "assigned"),
			Resource: role,
		}

		grantAnnotations, err := client.Grant(ctx, user, &entitlement)
		require.Nil(t, err)
		require.Empty(t, grantAnnotations)

		grants, nextToken, grantsAnnotations, err := client.Grants(ctx, role, &pagination.Token{})
		require.Nil(t, err)
		test.AssertNoRatelimitAnnotations(t, grantsAnnotations)
		require.Equal(t, "", nextToken)
		require.Len(t, grants, 1)

		grant := v2.Grant{
			Entitlement: &entitlement,
			Principal:   user,
		}

		revokeAnnotations, err := client.Revoke(ctx, &grant)
		require.Nil(t, err)
		require.Empty(t, revokeAnnotations)
	})

	t.Run("should handle permission errors gracefully", func(t *testing.T) {
		mockGithub := mocks.NewMockGitHub()
		mockGithub.SimulateOrgRolePermErr = true

		githubOrganization, _, _, _, _ := mockGithub.Seed()

		githubClient := github.NewClient(mockGithub.Server())
		cache := newOrgNameCache(githubClient)
		client := orgRoleBuilder(githubClient, cache)

		organization, _ := organizationResource(ctx, githubOrganization, nil)

		// Test List with permission error
		resources, nextToken, annotations, err := client.List(ctx, organization.Id, &pagination.Token{})
		require.Nil(t, err)
		require.Empty(t, resources)
		require.Empty(t, nextToken)
		test.AssertNoRatelimitAnnotations(t, annotations)

		// Test Grants with permission error
		role, _ := orgRoleResource(ctx, &OrganizationRole{
			ID:          1,
			Name:        "Test Role",
			Description: "Test Role Description",
		}, organization)

		grants, nextToken, grantsAnnotations, err := client.Grants(ctx, role, &pagination.Token{})
		require.Nil(t, err)
		require.Empty(t, grants)
		require.Empty(t, nextToken)
		test.AssertNoRatelimitAnnotations(t, grantsAnnotations)
	})
}
