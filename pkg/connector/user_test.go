package connector

import (
	"context"
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

	t.Run("should get a list of users", func(t *testing.T) {
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
			graphQLClient,
			cache,
			[]string{organization.DisplayName},
			nil,
			nil,
		)

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
		require.Equal(t, "", results.NextPageToken)
		require.Len(t, users, 1)
		require.Equal(t, *githubUser.Login, users[0].Id.Resource)
	})
}
