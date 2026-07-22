package connector

import (
	"context"
	"slices"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
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
		client := RepositoryBuilder(githubClient, cache, false, false)

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

// TestRepositoryGrantsOrgBasePermissionExpansion is a regression test for the
// empty default_repository_permission bug: when GitHub omits the field (the
// credential lacks org-owner visibility), the connector used to assume "read"
// and emit expandable org-member pull grants on every repo — inventing access
// for every org member. With direct-collaborators-only enabled the base
// permission is required to produce correct grants, so an omitted field must
// hard-fail the sync instead of guessing. It drives Grants() end-to-end with
// direct-collaborators-only enabled.
//
// Seeded mock IDs: org 12, repo 34, user 56.
func TestRepositoryGrantsOrgBasePermissionExpansion(t *testing.T) {
	ctx := context.Background()

	memberEntitlementID := "org:12:member"

	// listAllGrants pages through Grants() until the page token is exhausted.
	listAllGrants := func(t *testing.T, builder *repositoryResourceType, repository *v2.Resource) []*v2.Grant {
		t.Helper()
		var grants []*v2.Grant
		pToken := pagination.Token{}
		for {
			nextGrants, results, err := builder.Grants(ctx, repository, resourceSdk.SyncOpAttrs{
				PageToken: pToken,
				Session:   &noOpSessionStore{},
			})
			require.NoError(t, err)
			grants = append(grants, nextGrants...)
			if results.NextPageToken == "" {
				return grants
			}
			pToken.Token = results.NextPageToken
		}
	}

	// memberExpansionGrants returns grants whose GrantExpandable annotation
	// expands the org:12:member entitlement — i.e. repo access inferred from
	// org membership via the base permission.
	memberExpansionGrants := func(t *testing.T, grants []*v2.Grant) []*v2.Grant {
		t.Helper()
		var rv []*v2.Grant
		for _, g := range grants {
			expandable := &v2.GrantExpandable{}
			annos := annotations.Annotations(g.GetAnnotations())
			found, err := annos.Pick(expandable)
			require.NoError(t, err)
			if found && slices.Contains(expandable.GetEntitlementIds(), memberEntitlementID) {
				rv = append(rv, g)
			}
		}
		return rv
	}

	setup := func(t *testing.T) (*mocks.MockGitHub, *repositoryResourceType, *v2.Resource) {
		t.Helper()
		mgh := mocks.NewMockGitHub()
		githubOrganization, githubRepository, _, githubUser, _, _ := mgh.Seed()
		// Seed a direct collaborator so the collaborator page succeeds.
		mgh.AddRepositoryCollaborator(githubRepository.GetID(), githubUser.GetID())

		githubClient := github.NewClient(mgh.Server())
		builder := RepositoryBuilder(githubClient, newOrgNameCache(githubClient), false, true /* directCollaboratorsOnly */)

		organization, err := organizationResource(ctx, githubOrganization, nil, false)
		require.NoError(t, err)
		repository, err := repositoryResource(ctx, githubRepository, organization.Id)
		require.NoError(t, err)
		return mgh, builder, repository
	}

	t.Run("missing default_repository_permission fails the sync", func(t *testing.T) {
		_, builder, repository := setup(t)
		// The seeded org omits default_repository_permission, like GitHub does
		// for credentials without admin:org / Organization Administration. We
		// can't know org members' repo access without it, so Grants must error
		// rather than guess in either direction.
		grants, _, err := builder.Grants(ctx, repository, resourceSdk.SyncOpAttrs{
			PageToken: pagination.Token{},
			Session:   &noOpSessionStore{},
		})
		require.ErrorContains(t, err, "default_repository_permission")
		require.Empty(t, grants,
			"an omitted default_repository_permission must not produce grants")
	})

	t.Run("read default_repository_permission still expands members to pull", func(t *testing.T) {
		mgh, builder, repository := setup(t)
		mgh.SetOrgDefaultRepoPermission(12, "read")
		grants := listAllGrants(t, builder, repository)

		memberGrants := memberExpansionGrants(t, grants)
		require.Len(t, memberGrants, 1, "base permission read should expand org members to exactly pull")
		require.Equal(t, "repository:34:pull:org:12", memberGrants[0].GetId())
	})
}

func TestOrgBasePermissionToRepoPermissions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		basePerm string
		want     []string
	}{
		{name: "none skips member expansion", basePerm: "none", want: nil},
		{name: "empty skips member expansion", basePerm: "", want: nil},
		{name: "read grants pull", basePerm: "read", want: []string{repoPermissionPull}},
		{name: "write grants pull triage push", basePerm: "write", want: []string{repoPermissionPull, repoPermissionTriage, repoPermissionPush}},
		{name: "admin grants all levels", basePerm: "admin", want: []string{repoPermissionPull, repoPermissionTriage, repoPermissionPush, repoPermissionMaintain, repoPermissionAdmin}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, orgBasePermissionToRepoPermissions(tc.basePerm))
		})
	}
}
