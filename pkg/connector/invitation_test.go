package connector

import (
	"context"
	"net/http"
	"testing"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/migueleliasweb/go-github-mock/src/mock"
	"github.com/stretchr/testify/require"

	"github.com/conductorone/baton-github/test/mocks"
)

const (
	invitationTestOrgID    = int64(12)
	invitationTestOrgLogin = "test-org-12"
)

// invitationListAll drives invitationResourceType.List in a loop, marshalling
// the NextPageToken back in each iteration the way the SDK does at runtime,
// and returns every resource the connector produced across all pages of both
// underlying endpoints. The 50-iteration cap is a circuit breaker that fails
// loudly if pagination ever fails to terminate.
func invitationListAll(t *testing.T, ctx context.Context, b *invitationResourceType, parentID *v2.ResourceId) []*v2.Resource {
	t.Helper()
	var (
		all   []*v2.Resource
		token string
	)
	for i := 0; i < 50; i++ {
		resources, results, err := b.List(ctx, parentID, resourceSdk.SyncOpAttrs{
			PageToken: pagination.Token{Token: token},
			Session:   &noOpSessionStore{},
		})
		require.NoError(t, err)
		require.NotNil(t, results)
		all = append(all, resources...)
		if results.NextPageToken == "" {
			return all
		}
		token = results.NextPageToken
	}
	t.Fatalf("invitation List did not terminate within 50 iterations")
	return nil
}

// orgByIDHandler responds to Organizations.GetByID with a fixed stub. The
// invitation builder's orgCache calls this to resolve the parent resource ID
// (a numeric string) to an org login for the URL path.
func orgByIDHandler() mock.MockBackendOption {
	return mock.WithRequestMatchHandler(
		mocks.GetOrganizationById,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(mock.MustMarshal(github.Organization{
				ID:    github.Ptr(invitationTestOrgID),
				Login: github.Ptr(invitationTestOrgLogin),
			}))
		}),
	)
}

func notFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
}

func TestInvitationListPagination(t *testing.T) {
	ctx := context.Background()
	parentID := &v2.ResourceId{
		ResourceType: resourceTypeOrg.Id,
		Resource:     "12",
	}

	// Fixed timestamps so expires_at assertions are deterministic.
	pendingCreated1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pendingCreated2 := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	pendingCreated3 := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	expiredFailedAt1 := time.Date(2026, 1, 4, 12, 0, 0, 0, time.UTC)
	expiredFailedAt2 := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	pendingPage1 := []*github.Invitation{
		{
			ID:        github.Ptr(int64(1001)),
			Login:     github.Ptr("alice"),
			Email:     github.Ptr("alice@example.com"),
			Inviter:   &github.User{Login: github.Ptr("admin")},
			CreatedAt: &github.Timestamp{Time: pendingCreated1},
		},
		{
			ID:        github.Ptr(int64(1002)),
			Login:     github.Ptr("bob"),
			Email:     github.Ptr("bob@example.com"),
			Inviter:   &github.User{Login: github.Ptr("admin")},
			CreatedAt: &github.Timestamp{Time: pendingCreated2},
		},
	}
	pendingPage2 := []*github.Invitation{
		{
			ID:        github.Ptr(int64(1003)),
			Login:     github.Ptr("carol"),
			Email:     github.Ptr("carol@example.com"),
			Inviter:   &github.User{Login: github.Ptr("admin")},
			CreatedAt: &github.Timestamp{Time: pendingCreated3},
		},
	}

	// failedPage1 mixes an actually-expired invitation with one whose
	// failed_reason is something else; only the expired one should be kept.
	failedPage1 := []*github.Invitation{
		{
			ID:           github.Ptr(int64(2001)),
			Email:        github.Ptr("dave-expired@example.com"),
			Inviter:      &github.User{Login: github.Ptr("admin")},
			CreatedAt:    &github.Timestamp{Time: pendingCreated1},
			FailedAt:     &github.Timestamp{Time: expiredFailedAt1},
			FailedReason: github.Ptr("expired"),
		},
		{
			ID:           github.Ptr(int64(2002)),
			Email:        github.Ptr("eve-inactive@example.com"),
			Inviter:      &github.User{Login: github.Ptr("admin")},
			CreatedAt:    &github.Timestamp{Time: pendingCreated1},
			FailedAt:     &github.Timestamp{Time: expiredFailedAt1},
			FailedReason: github.Ptr("user_was_inactive"),
		},
	}
	failedPage2 := []*github.Invitation{
		{
			ID:           github.Ptr(int64(2003)),
			Email:        github.Ptr("frank-expired@example.com"),
			Inviter:      &github.User{Login: github.Ptr("admin")},
			CreatedAt:    &github.Timestamp{Time: pendingCreated2},
			FailedAt:     &github.Timestamp{Time: expiredFailedAt2},
			FailedReason: github.Ptr("expired"),
		},
		{
			ID:           github.Ptr(int64(2004)),
			Email:        github.Ptr("grace-unexpected@example.com"),
			Inviter:      &github.User{Login: github.Ptr("admin")},
			CreatedAt:    &github.Timestamp{Time: pendingCreated2},
			FailedAt:     &github.Timestamp{Time: expiredFailedAt2},
			FailedReason: github.Ptr("unexpected_failure"),
		},
	}

	newBuilder := func(httpClient *http.Client) *invitationResourceType {
		gh := github.NewClient(httpClient)
		return invitationBuilder(invitationBuilderParams{
			client:   gh,
			orgCache: newOrgNameCache(gh),
			orgs:     []string{invitationTestOrgLogin},
		})
	}

	t.Run("walks both endpoints across page boundaries", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			orgByIDHandler(),
			mock.WithRequestMatchPages(
				mock.GetOrgsInvitationsByOrg,
				pendingPage1, pendingPage2,
			),
			mock.WithRequestMatchPages(
				mock.GetOrgsFailedInvitationsByOrg,
				failedPage1, failedPage2,
			),
		)

		got := invitationListAll(t, ctx, newBuilder(client), parentID)

		// 3 pending (alice/bob/carol) + 2 expired-failed (dave/frank).
		// user_was_inactive and unexpected_failure must be filtered out.
		require.Len(t, got, 5, "expected 3 pending + 2 expired-failed; non-expired failed_reasons must be dropped")

		byID := map[string]*v2.Resource{}
		for _, r := range got {
			byID[r.Id.Resource] = r
		}
		require.Contains(t, byID, "1001")
		require.Contains(t, byID, "1002")
		require.Contains(t, byID, "1003")
		require.Contains(t, byID, "2001")
		require.Contains(t, byID, "2003")
		require.NotContains(t, byID, "2002", "user_was_inactive must be dropped")
		require.NotContains(t, byID, "2004", "unexpected_failure must be dropped")

		// Pending resources carry status=pending and expires_at = created_at + 7d.
		aliceProfile := invitationProfile(t, byID["1001"])
		require.Equal(t, invitationStatusPendingAcceptance, aliceProfile["invitation_status"])
		require.Equal(t,
			pendingCreated1.Add(invitationLifetime).UTC().Format(time.RFC3339),
			aliceProfile["invitation_expires_at"],
		)

		// Expired resources carry status=expired and expires_at = failed_at.
		daveProfile := invitationProfile(t, byID["2001"])
		require.Equal(t, invitationStatusExpired, daveProfile["invitation_status"])
		require.Equal(t,
			expiredFailedAt1.UTC().Format(time.RFC3339),
			daveProfile["invitation_expires_at"],
		)
	})

	t.Run("pending 404 falls through to failed", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			orgByIDHandler(),
			mock.WithRequestMatchHandler(mock.GetOrgsInvitationsByOrg, notFoundHandler()),
			mock.WithRequestMatchPages(mock.GetOrgsFailedInvitationsByOrg, failedPage1),
		)

		got := invitationListAll(t, ctx, newBuilder(client), parentID)
		// Pending 404'd, only the one expired entry from failedPage1 survives.
		require.Len(t, got, 1)
		require.Equal(t, "2001", got[0].Id.Resource)
		require.Equal(t, invitationStatusExpired,
			invitationProfile(t, got[0])["invitation_status"])
	})

	t.Run("failed 404 terminates cleanly", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			orgByIDHandler(),
			mock.WithRequestMatchPages(mock.GetOrgsInvitationsByOrg, pendingPage1),
			mock.WithRequestMatchHandler(mock.GetOrgsFailedInvitationsByOrg, notFoundHandler()),
		)

		got := invitationListAll(t, ctx, newBuilder(client), parentID)
		// Pending: 2 invitations. Failed: 404, contributes nothing.
		require.Len(t, got, 2)
		require.Equal(t, invitationStatusPendingAcceptance,
			invitationProfile(t, got[0])["invitation_status"])
	})

	t.Run("both endpoints empty terminates without API errors", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			orgByIDHandler(),
			mock.WithRequestMatchPages(mock.GetOrgsInvitationsByOrg, []*github.Invitation{}),
			mock.WithRequestMatchPages(mock.GetOrgsFailedInvitationsByOrg, []*github.Invitation{}),
		)

		got := invitationListAll(t, ctx, newBuilder(client), parentID)
		require.Empty(t, got)
	})
}

// invitationProfile fetches the UserTrait profile from a synced invitation
// resource and returns it as a map for easy assertion.
func invitationProfile(t *testing.T, r *v2.Resource) map[string]any {
	t.Helper()
	ut, err := resourceSdk.GetUserTrait(r)
	require.NoError(t, err)
	require.NotNil(t, ut)
	require.NotNil(t, ut.Profile)
	return ut.Profile.AsMap()
}
