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

			nextGrants, results, err := client.Grants(ctx, repository, resourceSdk.SyncOpAttrs{PageToken: pToken})
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
