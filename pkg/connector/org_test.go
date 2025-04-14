package connector

import (
	"context"
	"testing"

	"github.com/conductorone/baton-github/test"
	"github.com/conductorone/baton-github/test/mocks"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
)

func TestOrganization(t *testing.T) {
	ctx := context.Background()

	t.Run("should grant and revoke entitlements", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()

		githubOrganization, _, _, githubUser, _ := mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := orgBuilder(githubClient, cache, nil, false)

		organization, _ := organizationResource(ctx, githubOrganization, nil, false)
		user, _ := userResource(ctx, githubUser, *githubUser.Email, nil)

		entitlement := v2.Entitlement{
			Id:       entitlement.NewEntitlementID(organization, orgRoleMember),
			Resource: organization,
		}

		grantAnnotations, err := client.Grant(ctx, user, &entitlement)
		require.Nil(t, err)
		require.Empty(t, grantAnnotations)

		grants, nextToken, grantsAnnotations, err := client.Grants(ctx, organization, &pagination.Token{})
		require.Nil(t, err)
		test.AssertNoRatelimitAnnotations(t, grantsAnnotations)
		require.Equal(t, "", nextToken)
		require.Len(t, grants, 2)

		grant := v2.Grant{
			Entitlement: &entitlement,
			Principal:   user,
		}

		revokeAnnotations, err := client.Revoke(ctx, &grant)
		require.Nil(t, err)
		require.Empty(t, revokeAnnotations)
	})
}
