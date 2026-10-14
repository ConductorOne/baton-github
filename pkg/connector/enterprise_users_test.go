package connector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/cli"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/conductorone/baton-github/pkg/customclient"
	"github.com/conductorone/baton-github/test/mocks"
)

// orgSAMLGraphQL answers an organization that has its own SAML provider, so the
// user list does not fall through to the consumed-licenses enrichment.
func orgSAMLGraphQL(t *testing.T) *githubv4.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "externalIdentities") {
			_, _ = w.Write([]byte(`{"data":{"organization":{"samlIdentityProvider":{"externalIdentities":{"edges":[]}}},` +
				`"rateLimit":{"limit":5000,"remaining":4999,"resetAt":"2026-09-18T23:00:00Z"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"organization":{"samlIdentityProvider":{"id":"saml","ssoUrl":"https://example.com/sso"}}}}`))
	}))
	t.Cleanup(srv.Close)

	return githubv4.NewEnterpriseClient(srv.URL, srv.Client())
}

// newTestEnterpriseUserBuilder returns a user syncer whose organization holds
// the seeded member and whose enterprise is served by stub.
func newTestEnterpriseUserBuilder(
	t *testing.T,
	stub *enterpriseStub,
	syncsEnterpriseRoles bool,
) (*userResourceType, *v2.Resource) {
	t.Helper()

	if stub.invitations == nil {
		stub.invitations = make(map[string]string)
	}
	enterpriseClient := newTestEnterpriseAdminClient(t, stub, testOrg)

	mgh := mocks.NewMockGitHub()
	githubOrganization, _, _, _, _, err := mgh.Seed()
	require.NoError(t, err)
	organization, err := organizationResource(context.Background(), githubOrganization, nil, false)
	require.NoError(t, err)

	ghClient := github.NewClient(mgh.Server())
	builder := UserBuilder(
		ghClient,
		orgSAMLGraphQL(t),
		newOrgNameCache(ghClient),
		[]string{githubOrganization.GetLogin()},
		nil,
		[]string{testEnterprise},
		func(context.Context, []string) (map[string]*customclient.EnterpriseAdminClient, error) {
			return map[string]*customclient.EnterpriseAdminClient{testEnterprise: enterpriseClient}, nil
		},
		syncsEnterpriseRoles,
	)

	return builder, organization
}

// drainUserList walks every page of the user list and returns how many times
// each resource ID was emitted.
func drainUserList(t *testing.T, builder *userResourceType, organization *v2.Resource) map[string]int {
	t.Helper()

	counts := map[string]int{}
	token := ""
	for page := 0; page < 8; page++ {
		users, results, err := builder.List(context.Background(), organization.Id, resourceSdk.SyncOpAttrs{
			PageToken: pagination.Token{Token: token},
			Session:   &noOpSessionStore{},
		})
		require.NoError(t, err)
		for _, user := range users {
			counts[user.GetId().GetResource()]++
		}
		token = results.NextPageToken
		if token == "" {
			return counts
		}
	}
	t.Fatal("the user list did not finish")

	return nil
}

func TestUserListEmitsEnterprisePrincipalsMissingFromTheOrg(t *testing.T) {
	t.Parallel()

	stub := &enterpriseStub{
		ownerPages: [][]enterpriseStubOwner{
			{{id: testLoginID, login: testLogin}},
			{{id: 999, login: "outsider"}},
		},
		memberPages: [][]enterpriseStubOwner{
			{{id: 1001, login: "invitee"}, {id: 1002, login: "bystander"}},
		},
		invitations: map[string]string{"invitee": "EAI_invitee"},
	}
	builder, organization := newTestEnterpriseUserBuilder(t, stub, true)

	counts := drainUserList(t, builder, organization)

	// An Owner the organization already emits is left to the member page, so
	// the account keeps the identity that page resolved for it.
	require.Equal(t, 1, counts[testLogin])
	// An Owner outside the organization is the dangling principal.
	require.Equal(t, 1, counts["999"])
	// A pending Owner invitee holds the same grant, so it needs the same user.
	require.Equal(t, 1, counts["1001"])
	// An enterprise member with no role and no invitation is named by no grant.
	require.Zero(t, counts["1002"])
}

// The plan is the order the walk runs in, and the bag is a stack, so queuing
// it has to come back out in that same order.
func TestEnterpriseUserPhasePlanRunsInOrder(t *testing.T) {
	t.Parallel()

	plan := enterpriseUserPhasePlan(
		[]string{"first", "second"},
		[]string{enterpriseUserOwnersPhase, enterpriseUserPendingPhase},
	)
	require.Equal(t, []pagination.PageState{
		{ResourceTypeID: enterpriseUserOwnersPhase, ResourceID: "first"},
		{ResourceTypeID: enterpriseUserPendingPhase, ResourceID: "first"},
		{ResourceTypeID: enterpriseUserOwnersPhase, ResourceID: "second"},
		{ResourceTypeID: enterpriseUserPendingPhase, ResourceID: "second"},
	}, plan)

	bag := &pagination.Bag{}
	bag.Push(pagination.PageState{ResourceTypeID: resourceTypeUser.Id})
	queueEnterpriseUserPhases(bag, plan)

	var drained []pagination.PageState
	for bag.Current() != nil {
		drained = append(drained, pagination.PageState{
			ResourceTypeID: bag.ResourceTypeID(),
			ResourceID:     bag.ResourceID(),
		})
		require.NoError(t, bag.Next(""))
	}
	require.Equal(t, plan, drained)
}

// Each configured organization runs the walk at the end of its members, so it
// can run more than once in one sync. Repeating it has to be a no-op: an
// account an organization emits is never emitted from the enterprise, and one
// it does not emit comes back identical.
func TestRepeatingTheEnterpriseWalkIsANoOp(t *testing.T) {
	t.Parallel()

	stub := &enterpriseStub{
		ownerPages: [][]enterpriseStubOwner{{
			{id: testLoginID, login: testLogin},
			{id: 999, login: "outsider"},
		}},
	}
	builder, organization := newTestEnterpriseUserBuilder(t, stub, true)

	first := drainUserList(t, builder, organization)
	ownerQueries := stub.ownerQueries
	require.Positive(t, ownerQueries)

	second := drainUserList(t, builder, organization)

	require.Equal(t, first, second)
	require.Equal(t, 1, second[testLogin])
	require.Equal(t, 1, second["999"])
	// The walk really did run again rather than being skipped by remembered state.
	require.Greater(t, stub.ownerQueries, ownerQueries)
}

func TestUserListSkipsEnterprisePrincipalsWhenTheAppIsNotSetUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mgh := mocks.NewMockGitHub()
	githubOrganization, _, _, _, _, err := mgh.Seed()
	require.NoError(t, err)
	organization, err := organizationResource(ctx, githubOrganization, nil, false)
	require.NoError(t, err)

	ghClient := github.NewClient(mgh.Server())
	builder := UserBuilder(
		ghClient,
		orgSAMLGraphQL(t),
		newOrgNameCache(ghClient),
		[]string{githubOrganization.GetLogin()},
		nil,
		[]string{testEnterprise},
		func(context.Context, []string) (map[string]*customclient.EnterpriseAdminClient, error) {
			return nil, enterpriseSetupError{fmt.Errorf("app is not installed")}
		},
		true,
	)

	users, results, err := builder.List(ctx, organization.Id, resourceSdk.SyncOpAttrs{
		Session: &noOpSessionStore{},
	})
	require.NoError(t, err)
	require.Empty(t, results.NextPageToken)
	require.Len(t, users, 1)
	require.Equal(t, testLogin, users[0].GetId().GetResource())
}

// Queuing the phases reads the enterprise, so this page can fail on one that
// is unreadable. The SDK answers NotFound by finishing the action with a
// warning and completing the sync, which would drop the members this page
// already fetched and make C1 delete them, so the page has to fail instead.
func TestAnUnreadableEnterpriseFailsTheUserPageRatherThanWarning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mgh := mocks.NewMockGitHub()
	githubOrganization, _, _, _, _, err := mgh.Seed()
	require.NoError(t, err)
	organization, err := organizationResource(ctx, githubOrganization, nil, false)
	require.NoError(t, err)

	ghClient := github.NewClient(mgh.Server())
	builder := UserBuilder(
		ghClient,
		orgSAMLGraphQL(t),
		newOrgNameCache(ghClient),
		[]string{githubOrganization.GetLogin()},
		nil,
		[]string{testEnterprise},
		func(context.Context, []string) (map[string]*customclient.EnterpriseAdminClient, error) {
			return nil, status.Error(codes.NotFound, "enterprise not found")
		},
		true,
	)

	_, _, err = builder.List(ctx, organization.Id, resourceSdk.SyncOpAttrs{
		Session: &noOpSessionStore{},
	})
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

// C1 can leave enterprise_role out of the selection. Without that type there
// are no enterprise grants to resolve, so the organization's members stay the
// whole user population and the enterprise APIs are never called.
func TestUserListLeavesEnterprisePrincipalsOutWhenTheRoleTypeIsNotSynced(t *testing.T) {
	t.Parallel()

	stub := &enterpriseStub{
		ownerPages: [][]enterpriseStubOwner{{{id: 999, login: "outsider"}}},
	}
	builder, organization := newTestEnterpriseUserBuilder(t, stub, false)

	users, results, err := builder.List(context.Background(), organization.Id, resourceSdk.SyncOpAttrs{
		Session: &noOpSessionStore{},
	})
	require.NoError(t, err)
	require.Empty(t, results.NextPageToken)
	require.Len(t, users, 1)
	require.Equal(t, testLogin, users[0].GetId().GetResource())
	require.Zero(t, stub.ownerQueries)
}

// The connector translates C1's resource type selection for the builders. The
// capabilities prototype carries no options at all, and the SDK reads that as
// every advertised type rather than none.
func TestWillSyncResourceType(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opts *cli.ConnectorOpts
		want bool
	}{
		{
			name: "no options at all",
			want: true,
		},
		{
			name: "no selection",
			opts: &cli.ConnectorOpts{},
			want: true,
		},
		{
			name: "selected",
			opts: &cli.ConnectorOpts{SyncResourceTypeIDs: []string{"user", "enterprise_role"}},
			want: true,
		},
		{
			name: "not selected",
			opts: &cli.ConnectorOpts{SyncResourceTypeIDs: []string{"user", "org"}},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gh := &GitHub{connectorOpts: tc.opts}
			require.Equal(t, tc.want, gh.willSyncResourceType(resourceTypeEnterpriseRole.Id))
		})
	}
}

// A personal access token reads the role holders from consumed licenses, so it
// queues that phase rather than the two the enterprise administration API
// serves.
func TestTokenAuthQueuesTheConsumedLicensesPhase(t *testing.T) {
	t.Parallel()

	builder := UserBuilder(nil, nil, nil, nil, nil, []string{testEnterprise}, nil, true)
	bag := &pagination.Bag{}
	bag.Push(pagination.PageState{ResourceTypeID: resourceTypeUser.Id})

	token, queued, err := builder.continueWithEnterprisePrincipals(context.Background(), bag)
	require.NoError(t, err)
	require.True(t, queued)

	next := &pagination.Bag{}
	require.NoError(t, next.Unmarshal(token))
	require.Equal(t, enterpriseUserLicensesPhase, next.ResourceTypeID())
	require.Equal(t, testEnterprise, next.ResourceID())
}

// The token path emits a licensed user who holds an enterprise role, and only
// that user: a seat without a role is named by no grant, and a role holder the
// organization already emits keeps the identity that page resolved.
func TestLicensedRoleUsersCoverOnlyTheRoleHoldersTheOrgDoesNotEmit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Ratelimit-Limit", "5000")
		w.Header().Set("X-Ratelimit-Remaining", "4999")
		w.Header().Set("X-Ratelimit-Reset", "1900000000")
		switch r.URL.Path {
		case "/enterprises/" + testEnterprise + "/consumed-licenses":
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(`{"users":[` +
					`{"github_com_login":"outsider","github_com_enterprise_roles":["Owner"]},` +
					`{"github_com_login":"insider","github_com_enterprise_roles":["Owner"]},` +
					`{"github_com_login":"seat-only","github_com_enterprise_roles":[]}` +
					`]}`))
				return
			}
			_, _ = w.Write([]byte(`{"users":[]}`))
		case "/user/memberships/orgs/acme":
			_, _ = w.Write([]byte(`{"role":"admin","state":"active"}`))
		// The organization already emits this one, so it is left alone.
		case "/orgs/acme/members/insider":
			w.WriteHeader(http.StatusNoContent)
		case "/orgs/acme/members/outsider", "/orgs/acme/members/seat-only":
			w.WriteHeader(http.StatusNotFound)
		case "/users/outsider":
			_, _ = w.Write([]byte(`{"id":999,"login":"outsider"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL + "/")
	require.NoError(t, err)
	ghClient := github.NewClient(srv.Client())
	ghClient.BaseURL = base

	builder := UserBuilder(ghClient, nil, nil, []string{"acme"}, customclient.New(ghClient),
		[]string{testEnterprise}, nil, true)

	users, next, _, err := builder.listLicensedRoleUsers(ctx, []string{"acme"}, testEnterprise, "")
	require.NoError(t, err)
	require.Equal(t, "2", next)
	require.Len(t, users, 1)
	require.Equal(t, "999", users[0].GetId().GetResource())

	done, next, _, err := builder.listLicensedRoleUsers(ctx, []string{"acme"}, testEnterprise, "2")
	require.NoError(t, err)
	require.Empty(t, next)
	require.Empty(t, done)
}

// A deployment that names no organization syncs the ones it administers and
// silently leaves out the rest. Treating a principal as emitted by one of
// those would drop the user and leave the grant pointing at nothing, so only
// the administered organizations decide it.
func TestOnlyAdministeredOrgsDecideWhetherAPrincipalIsAlreadyEmitted(t *testing.T) {
	t.Parallel()

	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/orgs":
			_, _ = w.Write([]byte(`[{"login":"administered"},{"login":"joined"},{"login":"opaque"}]`))
		case "/user/memberships/orgs/administered":
			_, _ = w.Write([]byte(`{"role":"admin","state":"active"}`))
		// A member-only organization: the sync never emits its members.
		case "/user/memberships/orgs/joined":
			_, _ = w.Write([]byte(`{"role":"member","state":"active"}`))
		// Membership that is not visible reads the same way as not administered.
		case "/user/memberships/orgs/opaque":
			w.WriteHeader(http.StatusForbidden)
		case "/orgs/administered/members/outsider":
			w.WriteHeader(http.StatusNotFound)
		case "/enterprises/" + testEnterprise + "/consumed-licenses":
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(
					`{"users":[{"github_com_login":"outsider","github_com_enterprise_roles":["Owner"]}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"users":[]}`))
		case "/users/outsider":
			_, _ = w.Write([]byte(`{"id":999,"login":"outsider"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL + "/")
	require.NoError(t, err)
	ghClient := github.NewClient(srv.Client())
	ghClient.BaseURL = base

	builder := UserBuilder(ghClient, nil, nil, nil, customclient.New(ghClient),
		[]string{testEnterprise}, nil, true)

	ctx := context.Background()
	orgs, _, err := builder.syncedOrgs(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"administered"}, orgs)

	users, _, _, err := builder.listLicensedRoleUsers(ctx, orgs, testEnterprise, "")
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, "999", users[0].GetId().GetResource())
	require.NotContains(t, asked, "/orgs/joined/members/outsider")
	require.NotContains(t, asked, "/orgs/opaque/members/outsider")
}

// The enterprise query already supplied a stable ID, so an account that no
// longer resolves is still emitted rather than left as a dangling principal.
func TestEnterprisePrincipalIsEmittedWhenTheAccountDoesNotResolve(t *testing.T) {
	t.Parallel()

	mgh := mocks.NewMockGitHub()
	_, _, _, _, _, err := mgh.Seed()
	require.NoError(t, err)

	builder := UserBuilder(github.NewClient(mgh.Server()), nil, nil, nil, nil,
		[]string{testEnterprise}, nil, true)

	ur, _, err := builder.userFromDatabaseID(context.Background(), 999, "ghost")
	require.NoError(t, err)
	require.Equal(t, "999", ur.GetId().GetResource())
	require.Equal(t, "ghost", ur.GetDisplayName())
}
