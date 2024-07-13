package connector

import (
	"context"
	"testing"

	"github.com/conductorone/baton-github/test"
	"github.com/conductorone/baton-github/test/mocks"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/google/go-github/v63/github"
	"github.com/stretchr/testify/require"
)

func TestTeam(t *testing.T) {
	ctx := context.Background()

	t.Run("should grant and revoke entitlements", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()

		githubOrganization, githubTeam, githubUser, err := mgh.Seed()

		client := teamBuilder(
			github.NewClient(mgh.Server()),
			&orgNameCache{},
		)

		organization, err := organizationResource(ctx, githubOrganization, nil)
		team, err := teamResource(githubTeam, organization.Id)
		user, err := userResource(ctx, githubUser, *githubUser.Email, nil)

		entitlement := v2.Entitlement{
			Resource: team,
			//Id:       team.Id.String(), TODO MARCOS delete this?
		}

		grantAnnotations, err := client.Grant(ctx, user, &entitlement)
		require.Nil(t, err)
		require.Empty(t, grantAnnotations)

		grants, nextToken, grantsAnnotations, err := client.Grants(ctx, team, &pagination.Token{})
		require.Nil(t, err)
		test.AssertNoRatelimitAnnotations(t, grantsAnnotations)
		require.Equal(t, "", nextToken)
		require.Len(t, grants, 1)

		grant := v2.Grant{
			Entitlement: &entitlement,
			Principal:   user,
			//Id:          grants[0].Id,
		}

		revokeAnnotations, err := client.Revoke(ctx, &grant)
		require.Nil(t, err)
		require.Empty(t, revokeAnnotations)
	})
}
