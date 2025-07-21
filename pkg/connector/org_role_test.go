package connector

import (
	"context"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	entitlement2 "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"

	"github.com/conductorone/baton-github/test"
	"github.com/conductorone/baton-github/test/mocks"
)

func TestOrgRole(t *testing.T) {
	ctx := context.Background()

	t.Run("should grant and revoke entitlements", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()

		githubOrganization, _, _, githubUser, orgRole, _ := mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := orgRoleBuilder(githubClient, cache)

		organization, _ := organizationResource(ctx, githubOrganization, nil, true)
		roleResource, _ := orgRoleResource(ctx, &OrganizationRole{
			ID:          orgRole.ID,
			Name:        orgRole.Name,
			Description: orgRole.Description,
		}, organization)
		user, _ := userResource(ctx, githubUser, *githubUser.Email, nil)

		entitlement := v2.Entitlement{
			Id:       entitlement2.NewEntitlementID(roleResource, "assigned"),
			Resource: roleResource,
		}

		// Grant the role to the user
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

			nextGrants, nextToken, grantsAnnotations, err := client.Grants(ctx, roleResource, &pToken)
			grants = append(grants, nextGrants...)

			require.Nil(t, err)
			test.AssertHasRatelimitAnnotations(t, grantsAnnotations)
			if nextToken == "" {
				break
			}

			err = bag.Unmarshal(nextToken)
			if err != nil {
				t.Error(err)
			}
		}

		require.Len(t, grants, 2)

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

		githubOrganization, _, _, _, orgRole, _ := mockGithub.Seed()

		githubClient := github.NewClient(mockGithub.Server())
		cache := newOrgNameCache(githubClient)
		client := orgRoleBuilder(githubClient, cache)

		organization, _ := organizationResource(ctx, githubOrganization, nil, true)

		// Test List with permission error
		resources, nextToken, annotations, err := client.List(ctx, organization.Id, &pagination.Token{})
		require.Nil(t, err)
		require.Empty(t, resources)
		require.Empty(t, nextToken)
		test.AssertHasRatelimitAnnotations(t, annotations)

		// Test Grants with permission error
		role, _ := orgRoleResource(ctx, &OrganizationRole{
			ID:          orgRole.ID,
			Name:        orgRole.Name,
			Description: orgRole.Description,
		}, organization)

		grants, nextToken, grantsAnnotations, err := client.Grants(ctx, role, &pagination.Token{})
		require.Nil(t, err)
		require.Empty(t, grants)
		// The token should contain the initial state for users
		require.NotEmpty(t, nextToken)
		test.AssertHasRatelimitAnnotations(t, grantsAnnotations)
	})
}
