package connector

import (
	"context"
	"testing"

	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/conductorone/baton-github/test/mocks"
)

// TestValidateBasePermissionVisibility covers the up-front credential probe for
// direct-collaborators-only: default_repository_permission must be readable or
// validation fails with PermissionDenied. Seeded mock org ID is 12 (login
// "organization-12"), and the seeded org omits default_repository_permission,
// matching what GitHub returns to credentials without org-owner visibility.
func TestValidateBasePermissionVisibility(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T, directCollaboratorsOnly bool) (*mocks.MockGitHub, *GitHub) {
		t.Helper()
		mgh := mocks.NewMockGitHub()
		_, _, _, _, _, err := mgh.Seed()
		require.NoError(t, err)
		return mgh, &GitHub{
			client:                  github.NewClient(mgh.Server()),
			directCollaboratorsOnly: directCollaboratorsOnly,
		}
	}

	t.Run("missing default_repository_permission fails validation", func(t *testing.T) {
		_, gh := setup(t, true)
		err := gh.validateBasePermissionVisibility(ctx, "organization-12")
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
		require.ErrorContains(t, err, "default_repository_permission")
	})

	t.Run("visible default_repository_permission passes validation", func(t *testing.T) {
		mgh, gh := setup(t, true)
		mgh.SetOrgDefaultRepoPermission(12, "none")
		require.NoError(t, gh.validateBasePermissionVisibility(ctx, "organization-12"))
	})

	t.Run("skipped when direct-collaborators-only is off", func(t *testing.T) {
		_, gh := setup(t, false)
		// No API call is made, so even the missing field is fine.
		require.NoError(t, gh.validateBasePermissionVisibility(ctx, "organization-12"))
	})
}
