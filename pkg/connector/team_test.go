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

func TestTeam(t *testing.T) {
	ctx := context.Background()

	t.Run("should grant and revoke entitlements", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()

		githubOrganization, _, githubTeam, githubUser, _, _ := mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := TeamBuilder(githubClient, cache, false)

		organization, _ := organizationResource(ctx, githubOrganization, nil, false)
		team, _ := teamResource(githubTeam, githubOrganization.GetID(), organization.Id)
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
