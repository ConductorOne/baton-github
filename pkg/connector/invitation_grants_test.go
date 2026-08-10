package connector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	entitlementSdk "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/conductorone/baton-sdk/pkg/types/sessions"
	"github.com/google/go-github/v69/github"
	"github.com/migueleliasweb/go-github-mock/src/mock"
	"github.com/stretchr/testify/require"

	"github.com/conductorone/baton-github/test/mocks"
)

const (
	grantsTestOrgID    = int64(12)
	grantsTestOrgLogin = "test-org-12"
	grantsTestTeamID   = int64(78)
	grantsTestRepoID   = int64(34)
	grantsTestRepoName = "repository-34"
)

// memorySessionStore is a minimal in-process SessionStore so tests exercise the
// same caching path a real sync uses.
type memorySessionStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemorySessionStore() *memorySessionStore {
	return &memorySessionStore{data: map[string][]byte{}}
}

func (m *memorySessionStore) Get(_ context.Context, key string, _ ...sessions.SessionStoreOption) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	return v, ok, nil
}

func (m *memorySessionStore) GetMany(_ context.Context, keys []string, _ ...sessions.SessionStoreOption) (map[string][]byte, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := map[string][]byte{}
	var missing []string
	for _, k := range keys {
		if v, ok := m.data[k]; ok {
			found[k] = v
		} else {
			missing = append(missing, k)
		}
	}
	return found, missing, nil
}

func (m *memorySessionStore) Set(_ context.Context, key string, value []byte, _ ...sessions.SessionStoreOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *memorySessionStore) SetMany(_ context.Context, values map[string][]byte, _ ...sessions.SessionStoreOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range values {
		m.data[k] = v
	}
	return nil
}

func (m *memorySessionStore) Delete(_ context.Context, key string, _ ...sessions.SessionStoreOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *memorySessionStore) Clear(_ context.Context, _ ...sessions.SessionStoreOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = map[string][]byte{}
	return nil
}

func (m *memorySessionStore) GetAll(_ context.Context, _ string, _ ...sessions.SessionStoreOption) (map[string][]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range m.data {
		out[k] = v
	}
	return out, "", nil
}

// respondWith registers an endpoint that always marshals payload back.
func respondWith(endpoint mock.EndpointPattern, payload any) mock.MockBackendOption {
	return mock.WithRequestMatchHandler(endpoint, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(mock.MustMarshal(payload))
	}))
}

// pathTail returns the last path segment, which for the membership and
// collaborator endpoints is the username the connector asked GitHub to act on.
func pathTail(p string) string {
	parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
	return parts[len(parts)-1]
}

func pathTailInt(p string) int64 {
	v, _ := strconv.ParseInt(pathTail(p), 10, 64)
	return v
}

// grantsResourceSyncer is the slice of ResourceSyncerV2 the grant-walking helper
// needs; declared locally so the helper works for org, team, and repository.
type grantsResourceSyncer interface {
	Grants(ctx context.Context, resource *v2.Resource, opts resourceSdk.SyncOpAttrs) ([]*v2.Grant, *resourceSdk.SyncOpResults, error)
}

// collectGrants drives Grants to exhaustion the way the SDK does at runtime,
// feeding each NextPageToken back in. The iteration cap fails loudly rather than
// hanging if a new pagination state ever fails to terminate.
func collectGrants(t *testing.T, ctx context.Context, syncer grantsResourceSyncer, resource *v2.Resource, ss sessions.SessionStore) []*v2.Grant {
	t.Helper()
	var (
		all   []*v2.Grant
		token string
	)
	for i := 0; i < 50; i++ {
		grants, results, err := syncer.Grants(ctx, resource, resourceSdk.SyncOpAttrs{
			PageToken: pagination.Token{Token: token},
			Session:   ss,
		})
		require.NoError(t, err)
		require.NotNil(t, results)
		all = append(all, grants...)
		if results.NextPageToken == "" {
			return all
		}
		token = results.NextPageToken
	}
	t.Fatalf("Grants did not terminate within 50 iterations")
	return nil
}

func grantsTestOrgHandler() mock.MockBackendOption {
	return respondWith(mocks.GetOrganizationById, github.Organization{
		ID:    github.Ptr(grantsTestOrgID),
		Login: github.Ptr(grantsTestOrgLogin),
	})
}

func grantsTestTeamResource(t *testing.T) *v2.Resource {
	t.Helper()
	team, err := teamResource(
		&github.Team{ID: github.Ptr(grantsTestTeamID)},
		grantsTestOrgID,
		&v2.ResourceId{ResourceType: resourceTypeOrg.Id, Resource: "12"},
	)
	require.NoError(t, err)
	return team
}

// grantPrincipalKey identifies a grant's principal as "<resource type>:<id>".
func grantPrincipalKey(g *v2.Grant) string {
	return g.GetPrincipal().GetId().GetResourceType() + ":" + g.GetPrincipal().GetId().GetResource()
}

// grantEntitlementSlug pulls the permission off a grant's entitlement ID, which
// grant.NewGrant formats as "<resource type>:<resource id>:<slug>".
func grantEntitlementSlug(g *v2.Grant) string {
	return pathTailAfter(g.GetEntitlement().GetId(), ":")
}

func pathTailAfter(s, sep string) string {
	parts := strings.Split(s, sep)
	return parts[len(parts)-1]
}

func grantIDs(grants []*v2.Grant) map[string]string {
	out := map[string]string{}
	for _, g := range grants {
		out[grantPrincipalKey(g)] = grantEntitlementSlug(g)
	}
	return out
}

func TestTeamPendingInvitationGrants(t *testing.T) {
	ctx := context.Background()
	team := grantsTestTeamResource(t)

	client := mock.NewMockedHTTPClient(
		grantsTestOrgHandler(),
		// No accepted members; both role passes come back empty.
		respondWith(mocks.GetOrganizationsTeamsMembersByTeamId, []github.User{}),
		respondWith(mocks.GetOrganizationsTeamsInvitationsByTeamId, []*github.Invitation{
			{
				ID:    github.Ptr(int64(1001)),
				Login: github.Ptr("alice"),
				Email: github.Ptr("alice@example.com"),
				Role:  github.Ptr(invitationRoleDirectMember),
			},
			{
				// Email-only invitation: GitHub never resolved an account, so the
				// team role cannot be looked up and must default to member.
				ID:    github.Ptr(int64(1002)),
				Email: github.Ptr("bob@example.com"),
				Role:  github.Ptr(invitationRoleDirectMember),
			},
		}),
		respondWith(mocks.GetOrganizationsTeamsMembershipsByTeamIdByUsername, github.Membership{
			Role:  github.Ptr(teamRoleMaintainer),
			State: github.Ptr("pending"),
		}),
	)

	builder := TeamBuilder(github.NewClient(client), newOrgNameCache(github.NewClient(client)), false, false)
	grants := collectGrants(t, ctx, builder, team, newMemorySessionStore())

	require.Len(t, grants, 2)
	require.Equal(t, map[string]string{
		"invitation:1001": teamRoleMaintainer,
		"invitation:1002": teamRoleMember,
	}, grantIDs(grants))
}

func TestTeamGrantToInvitation(t *testing.T) {
	ctx := context.Background()
	team := grantsTestTeamResource(t)
	en := &v2.Entitlement{
		Id:       entitlementSdk.NewEntitlementID(team, teamRoleMember),
		Slug:     teamRoleMember,
		Resource: team,
	}
	invitationPrincipal := func(id string) *v2.Resource {
		return &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeInvitation.Id, Resource: id}}
	}

	namedInvitation := []*github.Invitation{{
		ID:    github.Ptr(int64(1001)),
		Login: github.Ptr("alice"),
		Email: github.Ptr("alice@example.com"),
		Role:  github.Ptr(invitationRoleDirectMember),
	}}
	emailOnlyInvitation := []*github.Invitation{{
		ID:    github.Ptr(int64(2001)),
		Email: github.Ptr("bob@example.com"),
		Role:  github.Ptr(invitationRoleDirectMember),
	}}

	t.Run("named invitee is added to the team directly", func(t *testing.T) {
		var addedUsername string
		client := mock.NewMockedHTTPClient(
			grantsTestOrgHandler(),
			respondWith(mock.GetOrgsInvitationsByOrg, namedInvitation),
			mock.WithRequestMatchHandler(
				mocks.PutOrganizationsTeamsMembershipsByOrganizationByTeamIdByUsername,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					addedUsername = pathTail(r.URL.Path)
					_, _ = w.Write(mock.MustMarshal(github.Membership{
						Role:  github.Ptr(teamRoleMember),
						State: github.Ptr("pending"),
					}))
				}),
			),
		)

		gh := github.NewClient(client)
		builder := TeamBuilder(gh, newOrgNameCache(gh), false, false)

		annos, err := builder.Grant(ctx, invitationPrincipal("1001"), en)
		require.NoError(t, err)
		require.Empty(t, annos)
		require.Equal(t, "alice", addedUsername, "the pending invitee's login must be what GitHub is asked to add")
	})

	t.Run("email-only invitee is refused when re-inviting is off", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			grantsTestOrgHandler(),
			respondWith(mock.GetOrgsInvitationsByOrg, emailOnlyInvitation),
			respondWith(mock.GetOrgsInvitationsTeamsByOrgByInvitationId, []*github.Team{}),
		)

		gh := github.NewClient(client)
		builder := TeamBuilder(gh, newOrgNameCache(gh), false, false)

		_, err := builder.Grant(ctx, invitationPrincipal("2001"), en)
		require.Error(t, err)
		require.Contains(t, err.Error(), "github_username")
		require.Contains(t, err.Error(), "reinvite")
	})

	t.Run("email-only invitee is re-invited with the team when enabled", func(t *testing.T) {
		var (
			cancelledID int64
			createBody  github.CreateOrgInvitationOptions
		)
		client := mock.NewMockedHTTPClient(
			grantsTestOrgHandler(),
			respondWith(mock.GetOrgsInvitationsByOrg, emailOnlyInvitation),
			// The invitation already carries one team, which must survive.
			respondWith(mock.GetOrgsInvitationsTeamsByOrgByInvitationId, []*github.Team{
				{ID: github.Ptr(int64(99))},
			}),
			mock.WithRequestMatchHandler(
				mock.DeleteOrgsInvitationsByOrgByInvitationId,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					cancelledID = pathTailInt(r.URL.Path)
					w.WriteHeader(http.StatusNoContent)
				}),
			),
			mock.WithRequestMatchHandler(
				mock.PostOrgsInvitationsByOrg,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					_ = json.Unmarshal(body, &createBody)
					_, _ = w.Write(mock.MustMarshal(github.Invitation{
						ID:    github.Ptr(int64(2002)),
						Email: github.Ptr("bob@example.com"),
					}))
				}),
			),
		)

		gh := github.NewClient(client)
		builder := TeamBuilder(gh, newOrgNameCache(gh), false, true)

		_, err := builder.Grant(ctx, invitationPrincipal("2001"), en)
		require.NoError(t, err)
		require.Equal(t, int64(2001), cancelledID, "the original invitation must be cancelled first")
		require.Equal(t, "bob@example.com", createBody.GetEmail())
		require.Equal(t, invitationRoleDirectMember, createBody.GetRole())
		require.ElementsMatch(t, []int64{99, grantsTestTeamID}, createBody.TeamID,
			"re-issuing must preserve the teams the invitation already had")
	})

	t.Run("already-attached team reports the grant as existing", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			grantsTestOrgHandler(),
			respondWith(mock.GetOrgsInvitationsByOrg, emailOnlyInvitation),
			respondWith(mock.GetOrgsInvitationsTeamsByOrgByInvitationId, []*github.Team{
				{ID: github.Ptr(grantsTestTeamID)},
			}),
		)

		gh := github.NewClient(client)
		builder := TeamBuilder(gh, newOrgNameCache(gh), false, false)

		annos, err := builder.Grant(ctx, invitationPrincipal("2001"), en)
		require.NoError(t, err)
		require.True(t, annos.Contains(&v2.GrantAlreadyExists{}))
	})

	t.Run("named invitee GitHub refuses reports the refusal, not a missing login", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			grantsTestOrgHandler(),
			respondWith(mock.GetOrgsInvitationsByOrg, namedInvitation),
			respondWith(mock.GetOrgsInvitationsTeamsByOrgByInvitationId, []*github.Team{}),
			mock.WithRequestMatchHandler(
				mocks.PutOrganizationsTeamsMembershipsByOrganizationByTeamIdByUsername,
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = w.Write([]byte(
						`{"message":"Validation Failed","errors":[{"message":"User isn't a member of this organization. Please invite them first."}]}`))
				}),
			),
		)

		gh := github.NewClient(client)
		builder := TeamBuilder(gh, newOrgNameCache(gh), false, false)

		_, err := builder.Grant(ctx, invitationPrincipal("1001"), en)
		require.Error(t, err)
		require.Contains(t, err.Error(), "GitHub refused")
		require.NotContains(t, err.Error(), "without a resolvable GitHub login")
	})

	t.Run("named invitee GitHub refuses is re-invited when enabled", func(t *testing.T) {
		var createBody github.CreateOrgInvitationOptions
		client := mock.NewMockedHTTPClient(
			grantsTestOrgHandler(),
			respondWith(mock.GetOrgsInvitationsByOrg, namedInvitation),
			respondWith(mock.GetOrgsInvitationsTeamsByOrgByInvitationId, []*github.Team{}),
			respondWith(mock.GetUsersByUsername, github.User{
				ID:    github.Ptr(int64(4242)),
				Login: github.Ptr("alice"),
			}),
			mock.WithRequestMatchHandler(
				mocks.PutOrganizationsTeamsMembershipsByOrganizationByTeamIdByUsername,
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = w.Write([]byte(
						`{"message":"Validation Failed","errors":[{"message":"User isn't a member of this organization. Please invite them first."}]}`))
				}),
			),
			mock.WithRequestMatchHandler(
				mock.DeleteOrgsInvitationsByOrgByInvitationId,
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}),
			),
			mock.WithRequestMatchHandler(
				mock.PostOrgsInvitationsByOrg,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					_ = json.Unmarshal(body, &createBody)
					_, _ = w.Write(mock.MustMarshal(github.Invitation{ID: github.Ptr(int64(1005))}))
				}),
			),
		)

		gh := github.NewClient(client)
		builder := TeamBuilder(gh, newOrgNameCache(gh), false, true)

		_, err := builder.Grant(ctx, invitationPrincipal("1001"), en)
		require.NoError(t, err)
		// A known login is re-invited by invitee_id, so the replacement invitation
		// keeps a resolvable GitHub account.
		require.Equal(t, int64(4242), createBody.GetInviteeID())
		require.Empty(t, createBody.GetEmail())
		require.ElementsMatch(t, []int64{grantsTestTeamID}, createBody.TeamID)
	})

	t.Run("invitation that is no longer pending fails with a retry hint", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			grantsTestOrgHandler(),
			respondWith(mock.GetOrgsInvitationsByOrg, []*github.Invitation{}),
		)

		gh := github.NewClient(client)
		builder := TeamBuilder(gh, newOrgNameCache(gh), false, false)

		_, err := builder.Grant(ctx, invitationPrincipal("9999"), en)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no longer pending")
	})
}

func TestOrgPendingInvitationGrants(t *testing.T) {
	ctx := context.Background()

	org, err := organizationResource(ctx, &github.Organization{
		ID:    github.Ptr(grantsTestOrgID),
		Login: github.Ptr(grantsTestOrgLogin),
	}, nil, false)
	require.NoError(t, err)

	client := mock.NewMockedHTTPClient(
		grantsTestOrgHandler(),
		respondWith(mock.GetOrgsByOrg, github.Organization{
			ID:    github.Ptr(grantsTestOrgID),
			Login: github.Ptr(grantsTestOrgLogin),
		}),
		// No accepted members; both role passes come back empty.
		respondWith(mock.GetOrgsMembersByOrg, []github.User{}),
		respondWith(mock.GetOrgsInvitationsByOrg, []*github.Invitation{
			{ID: github.Ptr(int64(1001)), Login: github.Ptr("alice"), Role: github.Ptr(invitationRoleAdmin)},
			{ID: github.Ptr(int64(1002)), Email: github.Ptr("bob@example.com"), Role: github.Ptr(invitationRoleDirectMember)},
			{ID: github.Ptr(int64(1003)), Email: github.Ptr("carol@example.com"), Role: github.Ptr("billing_manager")},
		}),
	)

	gh := github.NewClient(client)
	builder := OrgBuilder(gh, nil, newOrgNameCache(gh), nil, false, false)
	grants := collectGrants(t, ctx, builder, org, newMemorySessionStore())

	// The admin invitation emits admin + member, mirroring the accepted-member
	// pass; every other invitation role is plain membership.
	byPrincipal := map[string][]string{}
	for _, g := range grants {
		byPrincipal[grantPrincipalKey(g)] = append(byPrincipal[grantPrincipalKey(g)], grantEntitlementSlug(g))
	}
	require.Len(t, byPrincipal, 3)
	require.ElementsMatch(t, []string{orgRoleAdmin, orgRoleMember}, byPrincipal["invitation:1001"])
	require.Equal(t, []string{orgRoleMember}, byPrincipal["invitation:1002"])
	require.Equal(t, []string{orgRoleMember}, byPrincipal["invitation:1003"])
}

func TestRepositoryPendingInvitationGrants(t *testing.T) {
	ctx := context.Background()

	repo, err := repositoryResource(ctx, &github.Repository{
		ID:   github.Ptr(grantsTestRepoID),
		Name: github.Ptr(grantsTestRepoName),
	}, &v2.ResourceId{ResourceType: resourceTypeOrg.Id, Resource: "12"})
	require.NoError(t, err)

	client := mock.NewMockedHTTPClient(
		grantsTestOrgHandler(),
		respondWith(mock.GetReposCollaboratorsByOwnerByRepo, []github.User{}),
		respondWith(mock.GetReposTeamsByOwnerByRepo, []github.Team{}),
		respondWith(mock.GetReposInvitationsByOwnerByRepo, []*github.RepositoryInvitation{
			{
				ID:          github.Ptr(int64(555)),
				Invitee:     &github.User{Login: github.Ptr("Alice")},
				Permissions: github.Ptr("write"),
			},
			{
				// No pending org invitation: a direct outside-collaborator invite
				// has no invitation resource in the sync to hang a grant on.
				ID:          github.Ptr(int64(556)),
				Invitee:     &github.User{Login: github.Ptr("outsider")},
				Permissions: github.Ptr(readConst),
			},
			{
				// Expired invitations confer nothing.
				ID:          github.Ptr(int64(557)),
				Invitee:     &github.User{Login: github.Ptr("dave")},
				Permissions: github.Ptr("admin"),
				Expired:     github.Ptr(true),
			},
		}),
		respondWith(mock.GetOrgsInvitationsByOrg, []*github.Invitation{
			{ID: github.Ptr(int64(1001)), Login: github.Ptr("alice")},
			{ID: github.Ptr(int64(1004)), Login: github.Ptr("dave")},
		}),
	)

	gh := github.NewClient(client)
	builder := RepositoryBuilder(gh, newOrgNameCache(gh), false, false)
	grants := collectGrants(t, ctx, builder, repo, newMemorySessionStore())

	// "write" maps onto the repository entitlement vocabulary as "push", and the
	// invitee login is matched case-insensitively.
	require.Equal(t, map[string]string{"invitation:1001": repoPermissionPush}, grantIDs(grants))
}

func TestRepositoryGrantToInvitation(t *testing.T) {
	ctx := context.Background()

	repo, err := repositoryResource(ctx, &github.Repository{
		ID:   github.Ptr(grantsTestRepoID),
		Name: github.Ptr(grantsTestRepoName),
	}, &v2.ResourceId{ResourceType: resourceTypeOrg.Id, Resource: "12"})
	require.NoError(t, err)

	en := &v2.Entitlement{
		Id:       entitlementSdk.NewEntitlementID(repo, repoPermissionPush),
		Slug:     repoPermissionPush,
		Resource: repo,
	}
	principal := &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeInvitation.Id, Resource: "1001"}}

	repoByIDHandler := respondWith(mocks.GetRepositoryById, github.Repository{
		ID:    github.Ptr(grantsTestRepoID),
		Name:  github.Ptr(grantsTestRepoName),
		Owner: &github.User{Login: github.Ptr(grantsTestOrgLogin)},
		Organization: &github.Organization{
			ID:    github.Ptr(grantsTestOrgID),
			Login: github.Ptr(grantsTestOrgLogin),
		},
	})

	t.Run("named invitee is invited to the repository", func(t *testing.T) {
		var invitedUsername string
		client := mock.NewMockedHTTPClient(
			repoByIDHandler,
			respondWith(mock.GetOrgsInvitationsByOrg, []*github.Invitation{
				{ID: github.Ptr(int64(1001)), Login: github.Ptr("alice"), Email: github.Ptr("alice@example.com")},
			}),
			mock.WithRequestMatchHandler(
				mock.PutReposCollaboratorsByOwnerByRepoByUsername,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					invitedUsername = pathTail(r.URL.Path)
					_, _ = w.Write(mock.MustMarshal(github.RepositoryInvitation{ID: github.Ptr(int64(555))}))
				}),
			),
		)

		gh := github.NewClient(client)
		builder := RepositoryBuilder(gh, newOrgNameCache(gh), false, false)

		_, err := builder.Grant(ctx, principal, en)
		require.NoError(t, err)
		require.Equal(t, "alice", invitedUsername)
	})

	t.Run("email-only invitee cannot be given repository access", func(t *testing.T) {
		client := mock.NewMockedHTTPClient(
			repoByIDHandler,
			respondWith(mock.GetOrgsInvitationsByOrg, []*github.Invitation{
				{ID: github.Ptr(int64(1001)), Email: github.Ptr("bob@example.com")},
			}),
		)

		gh := github.NewClient(client)
		builder := RepositoryBuilder(gh, newOrgNameCache(gh), false, false)

		_, err := builder.Grant(ctx, principal, en)
		require.Error(t, err)
		require.Contains(t, err.Error(), "repository")
	})
}

func TestReinviteRestoresOriginalOnCreateFailure(t *testing.T) {
	ctx := context.Background()

	var createBodies []github.CreateOrgInvitationOptions
	client := mock.NewMockedHTTPClient(
		respondWith(mock.GetOrgsInvitationsTeamsByOrgByInvitationId, []*github.Team{
			{ID: github.Ptr(int64(99))},
		}),
		mock.WithRequestMatchHandler(
			mock.DeleteOrgsInvitationsByOrgByInvitationId,
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}),
		),
		mock.WithRequestMatchHandler(
			mock.PostOrgsInvitationsByOrg,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var opts github.CreateOrgInvitationOptions
				_ = json.Unmarshal(body, &opts)
				createBodies = append(createBodies, opts)

				// Fail the first create (the one carrying the new team) and let
				// the restore of the original team set succeed.
				if len(createBodies) == 1 {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = w.Write([]byte(`{"message":"Validation Failed"}`))
					return
				}
				_, _ = w.Write(mock.MustMarshal(github.Invitation{ID: github.Ptr(int64(3001))}))
			}),
		),
	)

	inv := &github.Invitation{
		ID:    github.Ptr(int64(2001)),
		Email: github.Ptr("bob@example.com"),
		Role:  github.Ptr(invitationRoleDirectMember),
	}

	_, err := reinviteWithTeams(ctx, github.NewClient(client), grantsTestOrgLogin, inv, []int64{99, grantsTestTeamID})
	require.Error(t, err)
	require.Len(t, createBodies, 2, "a failed re-invite must attempt to restore the original invitation")
	require.ElementsMatch(t, []int64{99, grantsTestTeamID}, createBodies[0].TeamID)
	require.ElementsMatch(t, []int64{99}, createBodies[1].TeamID,
		"the restore must put back exactly the teams the invitation started with")
}

func TestOrgRoleGrantRejectsInvitation(t *testing.T) {
	ctx := context.Background()

	gh := github.NewClient(mock.NewMockedHTTPClient())
	builder := OrgRoleBuilder(gh, newOrgNameCache(gh))

	_, err := builder.Grant(ctx,
		&v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeInvitation.Id, Resource: "1001"}},
		&v2.Entitlement{Id: "org_role:1:assigned", Resource: &v2.Resource{Id: &v2.ResourceId{Resource: "1"}}},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "accepted their org invitation")
}
