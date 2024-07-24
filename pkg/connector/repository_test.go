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

func TestRepository(t *testing.T) {
	ctx := context.Background()

	t.Run("should grant and revoke entitlements", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()

		githubOrganization, githubRepository, _, githubUser, _ := mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := repositoryBuilder(githubClient, cache)

		organization, _ := organizationResource(ctx, githubOrganization, nil)
		repository, _ := repositoryResource(ctx, githubRepository, organization.Id)
		user, _ := userResource(ctx, githubUser, *githubUser.Email, nil)

		entitlement := v2.Entitlement{Resource: repository}

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

			nextGrants, nextToken, grantsAnnotations, err := client.Grants(ctx, repository, &pToken)
			grants = append(grants, nextGrants...)

			require.Nil(t, err)
			test.AssertNoRatelimitAnnotations(t, grantsAnnotations)
			if nextToken == "" {
				break
			}

			err = bag.Unmarshal(nextToken)
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
