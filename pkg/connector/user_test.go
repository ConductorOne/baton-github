package connector

import (
	"context"
	"fmt"
	"testing"

	"github.com/conductorone/baton-github/test"
	"github.com/conductorone/baton-github/test/mocks"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
)

func TestUsersList(t *testing.T) {
	ctx := context.Background()

	trueBool, falseBool := true, false

	testCases := []struct {
		hasSamlEnabled *bool
		message        string
	}{
		{&trueBool, "true"},
		{&falseBool, "false"},
		{nil, "nil"},
	}
	for _, testCase := range testCases {
		t.Run(fmt.Sprintf("should get a list of users (SAML:%s)", testCase.message), func(t *testing.T) {
			mgh := mocks.NewMockGitHub()

			githubOrganization, _, _, githubUser, _, _ := mgh.Seed()

			organization, err := organizationResource(
				ctx,
				githubOrganization,
				nil,
				false,
			)
			if err != nil {
				t.Error(err)
			}

			githubClient := github.NewClient(mgh.Server())
			graphQLClient := mocks.MockGraphQL()
			cache := newOrgNameCache(githubClient)
			client := userBuilder(
				githubClient,
				testCase.hasSamlEnabled,
				graphQLClient,
				cache,
				[]string{organization.DisplayName},
			)

			// First call: fetches org members, transitions to outside collaborators phase.
			users, results, err := client.List(
				ctx,
				organization.Id,
				resourceSdk.SyncOpAttrs{
					PageToken: pagination.Token{},
					Session:   &noOpSessionStore{},
				},
			)
			require.Nil(t, err)
			test.AssertHasRatelimitAnnotations(t, results.Annotations)
			require.NotEmpty(t, results.NextPageToken)
			require.Len(t, users, 1)
			require.Equal(t, *githubUser.Login, users[0].Id.Resource)

			// Second call: fetches outside collaborators (none seeded), completes pagination.
			outsideCollabUsers, results, err := client.List(
				ctx,
				organization.Id,
				resourceSdk.SyncOpAttrs{
					PageToken: pagination.Token{Token: results.NextPageToken},
					Session:   &noOpSessionStore{},
				},
			)
			require.Nil(t, err)
			require.Equal(t, "", results.NextPageToken)
			require.Len(t, outsideCollabUsers, 0)
		})
	}
}

func TestUsersListWithOutsideCollaborators(t *testing.T) {
	ctx := context.Background()
	falseBool := false

	mgh := mocks.NewMockGitHub()
	githubOrganization, _, _, githubUser, _, _ := mgh.Seed()

	outsideCollabID := int64(99)
	outsideCollabLogin := "outside-collab-99"
	outsideCollabEmail := "99@example.com"
	outsideCollabUser := github.User{
		ID:    &outsideCollabID,
		Login: &outsideCollabLogin,
		Email: &outsideCollabEmail,
	}
	mgh.AddUser(outsideCollabUser)
	mgh.AddOutsideCollaborator(*githubOrganization.ID, outsideCollabID)

	organization, err := organizationResource(ctx, githubOrganization, nil, false)
	require.Nil(t, err)

	githubClient := github.NewClient(mgh.Server())
	graphQLClient := mocks.MockGraphQL()
	cache := newOrgNameCache(githubClient)
	client := userBuilder(githubClient, &falseBool, graphQLClient, cache, []string{organization.DisplayName})

	// Phase 1: org members.
	members, results, err := client.List(ctx, organization.Id, resourceSdk.SyncOpAttrs{
		PageToken: pagination.Token{},
		Session:   &noOpSessionStore{},
	})
	require.Nil(t, err)
	require.NotEmpty(t, results.NextPageToken)
	require.Len(t, members, 1)
	require.Equal(t, *githubUser.Login, members[0].Id.Resource)

	// Phase 2: outside collaborators.
	collabs, results, err := client.List(ctx, organization.Id, resourceSdk.SyncOpAttrs{
		PageToken: pagination.Token{Token: results.NextPageToken},
		Session:   &noOpSessionStore{},
	})
	require.Nil(t, err)
	require.Equal(t, "", results.NextPageToken)
	require.Len(t, collabs, 1)
	require.Equal(t, fmt.Sprintf("%d", outsideCollabID), collabs[0].Id.Resource)
}
