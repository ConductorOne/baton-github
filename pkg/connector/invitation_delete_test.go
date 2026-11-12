package connector

import (
	"context"
	"net/http"
	"strings"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/google/go-github/v69/github"
	"github.com/migueleliasweb/go-github-mock/src/mock"
	"github.com/stretchr/testify/require"
)

// TestInvitationDelete exercises invitationResourceType.Delete against the
// go-github-mock router (Pattern B: inline mock.NewMockedHTTPClient +
// github.NewClient). CancelInvite issues DELETE
// /orgs/{org}/invitations/{invitation_id}; getOrgs returns the configured
// orgs without any API call, so only the CancelInvite endpoint needs a
// handler. The already-absent marker (&v2.ResourceDoesNotExist{}) is asserted
// via annotations.Annotations.Contains.
func TestInvitationDelete(t *testing.T) {
	ctx := context.Background()

	newBuilder := func(httpClient *http.Client) *invitationResourceType {
		gh := github.NewClient(httpClient)
		return InvitationBuilder(InvitationBuilderParams{
			client:   gh,
			orgCache: newOrgNameCache(gh),
			orgs:     []string{invitationTestOrgLogin},
		})
	}

	invitationResID := func(resource string) *v2.ResourceId {
		return &v2.ResourceId{
			ResourceType: resourceTypeInvitation.Id,
			Resource:     resource,
		}
	}

	t.Run("confirmed absence emits marker", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			mock.WithRequestMatchHandler(
				mock.DeleteOrgsInvitationsByOrgByInvitationId,
				notFoundHandler(),
			),
		)

		annos, err := newBuilder(client).Delete(ctx, invitationResID("5551212"))
		require.NoError(t, err)
		require.True(t, annos.Contains(&v2.ResourceDoesNotExist{}),
			"a 404 with no active cancellation must emit the already-absent marker")
	})

	t.Run("ordinary success emits no marker", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			mock.WithRequestMatchHandler(
				mock.DeleteOrgsInvitationsByOrgByInvitationId,
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}),
			),
		)

		annos, err := newBuilder(client).Delete(ctx, invitationResID("5551212"))
		require.NoError(t, err)
		require.False(t, annos.Contains(&v2.ResourceDoesNotExist{}),
			"an active cancellation (204) is ordinary success and must carry no marker")
	})

	t.Run("other error stays error, no marker", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			mock.WithRequestMatchHandler(
				mock.DeleteOrgsInvitationsByOrgByInvitationId,
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				}),
			),
		)

		annos, err := newBuilder(client).Delete(ctx, invitationResID("5551212"))
		require.Error(t, err)
		require.Nil(t, annos, "a 500 is a real failure: no annotations and no marker")
	})

	t.Run("exact org and invitation id addressing preserved", func(t *testing.T) {
		var gotPath string
		client := mock.NewMockedHTTPClient(
			mock.WithRequestMatchHandler(
				mock.DeleteOrgsInvitationsByOrgByInvitationId,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotPath = r.URL.Path
					w.WriteHeader(http.StatusNoContent)
				}),
			),
		)

		annos, err := newBuilder(client).Delete(ctx, invitationResID("5551212"))
		require.NoError(t, err)
		require.Equal(t, "/orgs/test-org-12/invitations/5551212", gotPath,
			"CancelInvite must address org=test-org-12 and id=5551212")
		require.False(t, annos.Contains(&v2.ResourceDoesNotExist{}))
	})

	t.Run("multi-org: 404 plus a non-404 error withholds the marker but keeps success", func(t *testing.T) {
		// One configured org reports the invitation already absent (404) while
		// another returns a transient 5xx (state undetermined). The pre-existing
		// behavior returns success (isRemoved is set by the 404), but absence was
		// not authoritatively confirmed everywhere, so no marker is emitted.
		const secondOrg = "test-org-99"
		client := mock.NewMockedHTTPClient(
			mock.WithRequestMatchHandler(
				mock.DeleteOrgsInvitationsByOrgByInvitationId,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, secondOrg) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusNotFound)
				}),
			),
		)

		gh := github.NewClient(client)
		b := InvitationBuilder(InvitationBuilderParams{
			client:   gh,
			orgCache: newOrgNameCache(gh),
			orgs:     []string{invitationTestOrgLogin, secondOrg},
		})

		annos, err := b.Delete(ctx, invitationResID("5551212"))
		require.NoError(t, err)
		require.False(t, annos.Contains(&v2.ResourceDoesNotExist{}),
			"an unresolved non-404 error means absence was not confirmed: no marker")
	})

	t.Run("malformed invitation id errors before any API call", func(t *testing.T) {
		// No CancelInvite handler: the parse failure must short-circuit before
		// any HTTP request is issued.
		client := mock.NewMockedHTTPClient()

		annos, err := newBuilder(client).Delete(ctx, invitationResID("not-a-number"))
		require.Error(t, err)
		require.Nil(t, annos)
	})

	t.Run("wrong resource type errors", func(t *testing.T) {
		client := mock.NewMockedHTTPClient()

		annos, err := newBuilder(client).Delete(ctx, &v2.ResourceId{
			ResourceType: resourceTypeUser.Id,
			Resource:     "5551212",
		})
		require.Error(t, err)
		require.Nil(t, annos)
	})
}
