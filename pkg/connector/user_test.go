package connector

import (
	"context"
	"fmt"
	"testing"

	"github.com/conductorone/baton-github/test"
	"github.com/conductorone/baton-github/test/mocks"
	"github.com/conductorone/baton-sdk/pkg/pagination"
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

			githubOrganization, _, _, githubUser, _ := mgh.Seed()

			organization, err := organizationResource(
				ctx,
				githubOrganization,
				nil,
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
			)

			users, nextToken, annotations, err := client.List(
				ctx,
				organization.Id,
				&pagination.Token{},
			)
			require.Nil(t, err)
			test.AssertNoRatelimitAnnotations(t, annotations)
			require.Equal(t, "", nextToken)
			require.Len(t, users, 1)
			require.Equal(t, *githubUser.Login, users[0].Id.Resource)
		})
	}
}
