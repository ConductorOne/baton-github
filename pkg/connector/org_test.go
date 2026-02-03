package connector

import (
	"context"
	"testing"

	"github.com/conductorone/baton-github/test"
	"github.com/conductorone/baton-github/test/mocks"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
)

func TestOrganization(t *testing.T) {
	ctx := context.Background()

	t.Run("should grant and revoke entitlements", func(t *testing.T) {
		mgh := mocks.NewMockGitHub()

		githubOrganization, _, _, githubUser, _, _ := mgh.Seed()

		githubClient := github.NewClient(mgh.Server())
		cache := newOrgNameCache(githubClient)
		client := orgBuilder(githubClient, nil, cache, nil, false)

		organization, _ := organizationResource(ctx, githubOrganization, nil, false)
		user, _ := userResource(ctx, githubUser, *githubUser.Email, nil)

		entitlement := v2.Entitlement{
			Id:       entitlement.NewEntitlementID(organization, orgRoleMember),
			Resource: organization,
		}

		grantAnnotations, err := client.Grant(ctx, user, &entitlement)
		require.Nil(t, err)
		require.Empty(t, grantAnnotations)

		_, results, err := client.Grants(ctx, organization, resourceSdk.SyncOpAttrs{PageToken: pagination.Token{}})
		require.Nil(t, err)
		test.AssertHasRatelimitAnnotations(t, results.Annotations)
		require.Equal(t, "{\"states\":[{\"type\":\"admin\"}],\"current_state\":{\"type\":\"member\"}}", results.NextPageToken)

		grant := v2.Grant{
			Entitlement: &entitlement,
			Principal:   user,
		}

		revokeAnnotations, err := client.Revoke(ctx, &grant)
		require.Nil(t, err)
		require.Empty(t, revokeAnnotations)
	})
}
