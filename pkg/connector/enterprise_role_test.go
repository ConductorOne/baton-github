package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	entitlementSdk "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/conductorone/baton-github/test/mocks"
)

const (
	testEnterprise   = "example-enterprise"
	testEnterpriseID = "E_example"
	// The seeded mock user has ID 56 and login "56".
	testLogin   = "56"
	testLoginID = 56
	testOrg     = "example-org"
)

// enterpriseStub is an in-memory GitHub GraphQL enterprise: it answers the
// owner and invitation queries from its own state and applies the mutations to
// it, so the tests exercise the documents the connector actually sends.
type enterpriseStub struct {
	// ownerPages holds the users that currently hold the Owner role, split
	// into the pages the members connection returns.
	ownerPages [][]enterpriseStubOwner
	// invitations maps a login to its pending Owner invitation ID.
	invitations map[string]string
	// memberPages holds the enterprise member accounts, split into the pages
	// the members connection returns. They are the candidate set the pending
	// invitation lookup asks about.
	memberPages [][]enterpriseStubOwner

	// inviteErrorType makes inviteEnterpriseAdmin fail with that GraphQL error
	// type. UNPROCESSABLE is what GitHub returns for someone who already
	// administers the enterprise.
	inviteErrorType    string
	inviteErrorMessage string
	updateFails        bool
	cancelNotFound     bool
	// ownersRateLimited makes the owners read answer the way GitHub reports a
	// GraphQL budget error: HTTP 200 carrying errors[].
	ownersRateLimited bool
	// batchErrorType adds one entry of that type to the invitation batch,
	// alongside the NOT_FOUND entries the batch always produces.
	batchErrorType string
	// silentMutations make the mutations report success without changing any
	// state, which is how a phantom grant or revoke would look.
	silentMutations bool

	ownerQueries       int
	invitationQueries  int
	invitationBatches  int
	memberQueries      int
	organizationChecks int

	updatedRole  githubv4.EnterpriseAdministratorRole
	invitedLogin string
	cancelledID  string
}

type enterpriseStubOwner struct {
	id    int64
	login string
}

func (s *enterpriseStub) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(body.Query, "enterpriseAdministratorInvitation("):
		// The sync resolves many logins in one aliased request; Grant and
		// Revoke ask about a single login through the typed client.
		if _, single := body.Variables["login"]; single {
			s.writeInvitation(t, w, body.Variables)
		} else {
			s.writeInvitationBatch(t, w, body.Query, body.Variables)
		}

	case strings.Contains(body.Query, "enterpriseOwners("):
		require.Contains(t, body.Query, "databaseId")
		require.NotContains(t, body.Query, "members(",
			"owners must come from Organization.enterpriseOwners, not Enterprise.members")
		// An organizationRole filter would drop every enterprise owner whose
		// role in this organization is not OWNER, which is the bug this read
		// path replaced.
		require.NotContains(t, body.Query, "organizationRole",
			"the owners query must not filter by the owner's role in the organization")
		s.writeOwners(t, w, body.Variables)

	case strings.Contains(body.Query, "members("):
		s.writeMembers(t, w, body.Variables)

	case strings.Contains(body.Query, "organizations("):
		s.organizationChecks++
		_, _ = fmt.Fprintf(w, `{"data":{"enterprise":{"organizations":{"nodes":[{"login":%q}]}}}}`, testOrg)

	case strings.Contains(body.Query, "updateEnterpriseAdministratorRole("):
		requireMutationShape(t, body.Query, "updateEnterpriseAdministratorRole")
		input := mutationInput(t, body.Variables)
		login, _ := input["login"].(string)
		role, _ := input["role"].(string)
		if s.updateFails {
			_, _ = w.Write([]byte(`{"data":{"updateEnterpriseAdministratorRole":null},` +
				`"errors":[{"type":"RATE_LIMITED","message":"rate limit exceeded"}]}`))
			return
		}
		s.updatedRole = githubv4.EnterpriseAdministratorRole(role)
		if !s.silentMutations {
			if role == string(enterpriseAdministratorRoleUnaffiliated) {
				s.removeOwner(login)
			} else {
				s.ownerPages = [][]enterpriseStubOwner{{{id: testLoginID, login: login}}}
			}
		}
		_, _ = w.Write([]byte(`{"data":{"updateEnterpriseAdministratorRole":{"clientMutationId":null}}}`))

	case strings.Contains(body.Query, "inviteEnterpriseAdmin("):
		requireMutationShape(t, body.Query, "inviteEnterpriseAdmin")
		input := mutationInput(t, body.Variables)
		login, _ := input["invitee"].(string)
		if s.inviteErrorType != "" {
			message := s.inviteErrorMessage
			if message == "" {
				// Observed live against GitHub for an existing administrator.
				message = "Invitee is already an owner of this enterprise"
			}
			_, _ = fmt.Fprintf(w,
				`{"data":{"inviteEnterpriseAdmin":null},"errors":[{"type":%q,"message":%q}]}`,
				s.inviteErrorType, message)
			return
		}
		s.invitedLogin = login
		if !s.silentMutations {
			s.invitations[login] = "EAI_" + login
		}
		_, _ = w.Write([]byte(`{"data":{"inviteEnterpriseAdmin":{"clientMutationId":null}}}`))

	case strings.Contains(body.Query, "cancelEnterpriseAdminInvitation("):
		requireMutationShape(t, body.Query, "cancelEnterpriseAdminInvitation")
		input := mutationInput(t, body.Variables)
		id, _ := input["invitationId"].(string)
		s.cancelledID = id
		for login, invitation := range s.invitations {
			if invitation == id {
				delete(s.invitations, login)
			}
		}
		if s.cancelNotFound {
			_, _ = w.Write([]byte(`{"data":{"cancelEnterpriseAdminInvitation":null},` +
				`"errors":[{"type":"NOT_FOUND","message":"invitation not found"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"cancelEnterpriseAdminInvitation":{"clientMutationId":null}}}`))

	case strings.Contains(body.Query, "enterprise(slug: $slug){id}"):
		_, _ = fmt.Fprintf(w, `{"data":{"enterprise":{"id":%q}}}`, testEnterpriseID)

	default:
		t.Fatalf("unexpected GraphQL operation: %s", body.Query)
	}
}

func (s *enterpriseStub) removeOwner(login string) {
	for pageIndex, page := range s.ownerPages {
		remaining := make([]enterpriseStubOwner, 0, len(page))
		for _, owner := range page {
			if !strings.EqualFold(owner.login, login) {
				remaining = append(remaining, owner)
			}
		}
		s.ownerPages[pageIndex] = remaining
	}
}

func (s *enterpriseStub) writeOwners(t *testing.T, w http.ResponseWriter, variables map[string]any) {
	t.Helper()
	s.ownerQueries++

	if s.ownersRateLimited {
		_, err := w.Write([]byte(`{"data":{"organization":null},` +
			`"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		require.NoError(t, err)
		return
	}

	pageIndex := 0
	if after, ok := variables["after"].(string); ok && after != "" {
		_, err := fmt.Sscanf(after, "cursor-%d", &pageIndex)
		require.NoError(t, err)
	}

	owners := []enterpriseStubOwner{}
	if pageIndex < len(s.ownerPages) {
		owners = s.ownerPages[pageIndex]
	}
	hasNextPage := pageIndex+1 < len(s.ownerPages)

	nodes := make([]string, 0, len(owners))
	for _, owner := range owners {
		nodes = append(nodes, fmt.Sprintf(`{"databaseId":%d,"login":%q}`, owner.id, owner.login))
	}

	_, err := fmt.Fprintf(
		w,
		`{"data":{"organization":{"enterpriseOwners":{"nodes":[%s],`+
			`"pageInfo":{"hasNextPage":%t,"endCursor":"cursor-%d"}}},`+
			`"rateLimit":{"limit":5000,"remaining":4999,"resetAt":"2026-09-18T23:00:00Z"}}}`,
		strings.Join(nodes, ","), hasNextPage, pageIndex+1,
	)
	require.NoError(t, err)
}

// writeInvitation answers the way GitHub does: the invitation when there is
// one, and a NOT_FOUND error when there is not.
// writeMembers answers one page of the enterprise member accounts, in the
// EnterpriseUserAccount shape GitHub returns for this connection.
func (s *enterpriseStub) writeMembers(t *testing.T, w http.ResponseWriter, variables map[string]any) {
	t.Helper()
	s.memberQueries++

	pageIndex := 0
	if after, ok := variables["after"].(string); ok && after != "" {
		_, err := fmt.Sscanf(after, "member-cursor-%d", &pageIndex)
		require.NoError(t, err)
	}

	members := []enterpriseStubOwner{}
	if pageIndex < len(s.memberPages) {
		members = s.memberPages[pageIndex]
	}
	hasNextPage := pageIndex+1 < len(s.memberPages)

	nodes := make([]string, 0, len(members))
	for _, member := range members {
		// Only the selected fields come back, so no __typename here: the query
		// resolves the union with inline fragments instead.
		nodes = append(nodes, fmt.Sprintf(
			`{"login":%q,"user":{"databaseId":%d,"login":%q}}`,
			member.login, member.id, member.login))
	}

	_, err := fmt.Fprintf(w,
		`{"data":{"enterprise":{"members":{"nodes":[%s],`+
			`"pageInfo":{"hasNextPage":%t,"endCursor":"member-cursor-%d"}}},`+
			`"rateLimit":{"limit":5000,"remaining":4998,"resetAt":"2026-09-18T23:00:00Z"}}}`,
		strings.Join(nodes, ","), hasNextPage, pageIndex+1)
	require.NoError(t, err)
}

// writeInvitationBatch answers the aliased lookup the sync uses. Every login
// without an invitation contributes a NOT_FOUND entry to errors[] next to the
// aliases that did resolve, which is how GitHub answers a partial batch.
func (s *enterpriseStub) writeInvitationBatch(t *testing.T, w http.ResponseWriter, query string, variables map[string]any) {
	t.Helper()
	s.invitationBatches++

	require.NotContains(t, query, `userLogin: "`, "logins must travel as variables, not in the query text")
	require.Equal(t, string(githubv4.EnterpriseAdministratorRoleOwner), variables["role"])

	aliases, failures := []string{}, []string{}
	for i := 0; ; i++ {
		login, ok := variables[fmt.Sprintf("l%d", i)].(string)
		if !ok {
			break
		}
		alias := fmt.Sprintf("i%d", i)
		require.Contains(t, query, alias+": enterpriseAdministratorInvitation")
		if invitationID, invited := s.invitations[login]; invited {
			aliases = append(aliases, fmt.Sprintf(`%q:{"id":%q}`, alias, invitationID))
			continue
		}
		aliases = append(aliases, fmt.Sprintf(`%q:null`, alias))
		failures = append(failures, fmt.Sprintf(
			`{"type":"NOT_FOUND","message":"Could not resolve to a pending invitation for %s."}`, login))
	}

	if s.batchErrorType != "" {
		failures = append(failures, fmt.Sprintf(
			`{"type":%q,"message":"the app lost access to the enterprise"}`, s.batchErrorType))
	}

	aliases = append(aliases, `"rateLimit":{"limit":5000,"remaining":4997,"resetAt":"2026-09-18T23:00:00Z"}`)
	body := fmt.Sprintf(`{"data":{%s}`, strings.Join(aliases, ","))
	if len(failures) > 0 {
		body += fmt.Sprintf(`,"errors":[%s]`, strings.Join(failures, ","))
	}
	_, err := w.Write([]byte(body + "}"))
	require.NoError(t, err)
}

func (s *enterpriseStub) writeInvitation(t *testing.T, w http.ResponseWriter, variables map[string]any) {
	t.Helper()
	s.invitationQueries++

	login, ok := variables["login"].(string)
	require.True(t, ok, "invitation lookup must send the login as a variable")
	require.Equal(t, string(githubv4.EnterpriseAdministratorRoleOwner), variables["role"])

	invitationID, invited := s.invitations[login]
	if !invited {
		_, _ = fmt.Fprintf(w,
			`{"data":{"enterpriseAdministratorInvitation":null},`+
				`"errors":[{"type":"NOT_FOUND","message":"Could not resolve to an invitation for %s."}]}`, login)
		return
	}
	_, _ = fmt.Fprintf(w, `{"data":{"enterpriseAdministratorInvitation":{"id":%q}}}`, invitationID)
}

// requireMutationShape rejects the two mutation bodies GitHub answers with a
// 200 plus an errors array: a payload without a selection set, and a
// Query-only rateLimit field selected on Mutation.
func requireMutationShape(t *testing.T, query string, field string) {
	t.Helper()

	_, payload, ok := strings.Cut(query, field+"(input: $input)")
	require.True(t, ok, "mutation %s must take its input as a variable", field)
	require.True(t, strings.HasPrefix(payload, "{"), "mutation %s must select payload fields", field)
	require.NotEqual(t, "{}", strings.TrimSuffix(payload, "}"), "mutation %s selection set is empty", field)
	require.NotContains(t, query, "rateLimit", "rateLimit does not exist on type Mutation")
}

func mutationInput(t *testing.T, variables map[string]any) map[string]any {
	t.Helper()

	input, ok := variables["input"].(map[string]any)
	require.True(t, ok, "mutation must send its input as a variable")
	// The enterprise node ID is resolved once at construction. An empty one
	// here means that step was skipped, which GitHub would reject at runtime.
	if enterpriseID, present := input["enterpriseId"]; present {
		require.Equal(t, testEnterpriseID, enterpriseID,
			"mutation must carry the enterprise node ID resolved at construction")
	}
	return input
}

// requireNoIdempotencyClaim asserts the operation actually acted instead of
// reporting the state as already correct. Emptiness is not the assertion:
// every path also carries the GraphQL budget left after reading the owners.
func requireNoIdempotencyClaim(t *testing.T, annos annotations.Annotations) {
	t.Helper()

	var alreadyExists v2.GrantAlreadyExists
	var alreadyRevoked v2.GrantAlreadyRevoked
	require.False(t, annos.Contains(&alreadyExists))
	require.False(t, annos.Contains(&alreadyRevoked))
}

func newTestEnterpriseRoleBuilder(
	t *testing.T,
	stub *enterpriseStub,
) (*enterpriseRoleResourceType, *v2.Resource, *v2.Entitlement) {
	t.Helper()

	if stub.invitations == nil {
		stub.invitations = make(map[string]string)
	}

	graphqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.handle(t, w, r)
	}))
	t.Cleanup(graphqlSrv.Close)

	// Both installations point at the same stub: the test exercises the
	// queries, not the two-token split.
	enterpriseClient, err := newEnterpriseAdministratorClient(
		graphqlSrv.URL, graphqlSrv.Client(), graphqlSrv.Client(), testOrg)
	require.NoError(t, err)
	// Mirrors construction: the node ID is resolved once, not per operation.
	require.NoError(t, enterpriseClient.resolveEnterpriseNodeID(context.Background(), testEnterprise))

	mgh := mocks.NewMockGitHub()
	_, _, _, githubUser, _, err := mgh.Seed()
	require.NoError(t, err)

	builder := EnterpriseRoleBuilder(
		github.NewClient(mgh.Server()),
		nil,
		nil,
		[]string{testEnterprise},
		func(context.Context) (map[string]*githubEnterpriseAdministratorClient, error) {
			return map[string]*githubEnterpriseAdministratorClient{testEnterprise: enterpriseClient}, nil
		},
	)

	principalID, err := resourceSdk.NewResourceID(resourceTypeUser, githubUser.GetID())
	require.NoError(t, err)

	roleResource, err := resourceSdk.NewRoleResource(
		enterpriseRoleOwner,
		resourceTypeEnterpriseRole,
		testEnterprise+":"+enterpriseRoleOwner,
		[]resourceSdk.RoleTraitOption{},
	)
	require.NoError(t, err)

	ent := &v2.Entitlement{
		Id:       entitlementSdk.NewEntitlementID(roleResource, enterpriseRoleAssigned),
		Slug:     enterpriseRoleAssigned,
		Resource: roleResource,
	}

	return builder, &v2.Resource{Id: principalID}, ent
}

func TestEnterpriseRoleGrant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// updateEnterpriseAdministratorRole rejects anyone who is not already an
	// administrator, so a plain member can only be invited.
	// The invitation is reported as the grant it will become, which is what
	// the sync emits too: returning nothing here would make C1 drop an overlay
	// that the next sync puts straight back.
	t.Run("invites a member and reports the grant", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		grants, annos, err := builder.Grant(ctx, principal, ent)
		require.NoError(t, err)
		requireNoIdempotencyClaim(t, annos)
		require.Len(t, grants, 1)
		require.Equal(t, testLogin, grants[0].GetPrincipal().GetId().GetResource())
		require.Equal(t, testLogin, stub.invitedLogin)
		require.Empty(t, stub.updatedRole)
	})

	// A billing manager cannot be invited and is promoted in place instead.
	// Which case applies is unreadable for a GitHub App, so the connector
	// falls back once the invitation is rejected for that reason.
	t.Run("promotes an administrator the invitation rejected", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{inviteErrorType: "UNPROCESSABLE"}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		grants, annos, err := builder.Grant(ctx, principal, ent)
		require.NoError(t, err)
		requireNoIdempotencyClaim(t, annos)
		require.Len(t, grants, 1)
		require.Equal(t, githubv4.EnterpriseAdministratorRoleOwner, stub.updatedRole)
		require.Empty(t, stub.invitedLogin)
	})

	// Any other invitation failure must surface instead of triggering a second
	// mutation the user never asked for.
	t.Run("does not promote after an unrelated invitation failure", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{
			inviteErrorType:    "RATE_LIMITED",
			inviteErrorMessage: "rate limit exceeded",
		}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		_, _, err := builder.Grant(ctx, principal, ent)
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.Empty(t, stub.updatedRole)
	})

	// When both mutations fail, the promotion's status code is the one that
	// reaches C1: it decides whether the task is worth retrying.
	t.Run("surfaces the promotion status when both mutations fail", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{inviteErrorType: "UNPROCESSABLE", updateFails: true}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		_, _, err := builder.Grant(ctx, principal, ent)
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.ErrorContains(t, err, "promoting")
		// The invitation error is not interpolated into the message: it is
		// always the expected FailedPrecondition, and both errors carry the
		// connector prefix, so repeating it makes the message unreadable.
		require.Equal(t, 1, strings.Count(err.Error(), "baton-github:"))
	})

	t.Run("reports an owner as already granted", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{
			ownerPages: [][]enterpriseStubOwner{{{id: testLoginID, login: testLogin}}},
		}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		grants, annos, err := builder.Grant(ctx, principal, ent)
		require.NoError(t, err)
		require.Len(t, grants, 1)

		var alreadyExists v2.GrantAlreadyExists
		require.True(t, annos.Contains(&alreadyExists))
		require.Empty(t, stub.updatedRole)
		require.Empty(t, stub.invitedLogin)

		// Reading the owners spends GraphQL budget, so the remaining budget
		// travels with the idempotency annotation instead of being dropped.
		var rateLimit v2.RateLimitDescription
		require.True(t, annos.Contains(&rateLimit))
	})

	// The owner check has to page: the owners connection has no login filter.
	t.Run("finds an owner on a later page", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{
			ownerPages: [][]enterpriseStubOwner{
				{{id: 99, login: "another-owner"}},
				{{id: testLoginID, login: testLogin}},
			},
		}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		grants, annos, err := builder.Grant(ctx, principal, ent)
		require.NoError(t, err)
		require.Len(t, grants, 1)

		var alreadyExists v2.GrantAlreadyExists
		require.True(t, annos.Contains(&alreadyExists))
		require.Equal(t, 2, stub.ownerQueries)
		require.Empty(t, stub.invitedLogin)
	})

	// Re-inviting returns the same invitation, so a repeat grant must not send
	// a second one.
	t.Run("does not resend a pending invitation", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{invitations: map[string]string{testLogin: "EAI_existing"}}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		grants, annos, err := builder.Grant(ctx, principal, ent)
		require.NoError(t, err)
		require.Len(t, grants, 1)

		var alreadyExists v2.GrantAlreadyExists
		require.True(t, annos.Contains(&alreadyExists))
		require.Empty(t, stub.invitedLogin, "an existing invitation must not be sent again")
	})

	// The mutation reporting success is not evidence that GitHub applied it.
	// Without this guard C1 would record access that does not exist.
	t.Run("rejects a grant GitHub did not apply", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{silentMutations: true}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		grants, _, err := builder.Grant(ctx, principal, ent)
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.Empty(t, grants)
		require.Equal(t, testLogin, stub.invitedLogin)
	})

	t.Run("rejects a role other than owner", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)
		ent.Resource.Id.Resource = testEnterprise + ":Member"

		_, _, err := builder.Grant(ctx, principal, ent)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Empty(t, stub.invitedLogin)
	})
}

func TestEnterpriseRoleRevoke(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// UNAFFILIATED demotes the administrator but keeps enterprise membership;
	// removeEnterpriseAdmin would evict them from the enterprise.
	t.Run("demotes an active owner", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{
			ownerPages: [][]enterpriseStubOwner{{{id: testLoginID, login: testLogin}}},
		}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		annos, err := builder.Revoke(ctx, &v2.Grant{Principal: principal, Entitlement: ent})
		require.NoError(t, err)
		requireNoIdempotencyClaim(t, annos)
		require.Equal(t, enterpriseAdministratorRoleUnaffiliated, stub.updatedRole)
	})

	// Holding the role and carrying an invitation are not alternatives. If the
	// revoke only demoted, the invitation would survive and the verification
	// that follows would report a retryable failure for a demotion that had
	// already gone through.
	t.Run("clears both the role and a leftover invitation", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{
			ownerPages:  [][]enterpriseStubOwner{{{id: testLoginID, login: testLogin}}},
			invitations: map[string]string{testLogin: "EAI_leftover"},
		}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		annos, err := builder.Revoke(ctx, &v2.Grant{Principal: principal, Entitlement: ent})
		require.NoError(t, err)
		requireNoIdempotencyClaim(t, annos)
		require.Equal(t, enterpriseAdministratorRoleUnaffiliated, stub.updatedRole)
		require.Equal(t, "EAI_leftover", stub.cancelledID)
	})

	t.Run("cancels an invitation that was never accepted", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{invitations: map[string]string{testLogin: "EAI_existing"}}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		annos, err := builder.Revoke(ctx, &v2.Grant{Principal: principal, Entitlement: ent})
		require.NoError(t, err)
		requireNoIdempotencyClaim(t, annos)
		require.Equal(t, "EAI_existing", stub.cancelledID)
		require.Empty(t, stub.updatedRole)
	})

	// GitHub expires an invitation after seven days, so a time-bound revoke can
	// arrive once it is already gone.
	t.Run("tolerates an invitation that expired mid-revoke", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{
			invitations:    map[string]string{testLogin: "EAI_existing"},
			cancelNotFound: true,
		}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		annos, err := builder.Revoke(ctx, &v2.Grant{Principal: principal, Entitlement: ent})
		require.NoError(t, err)
		requireNoIdempotencyClaim(t, annos)
		require.Equal(t, "EAI_existing", stub.cancelledID)
	})

	// The mirror of the grant guard: reporting a revoke that GitHub did not
	// apply would let C1 believe the access is gone while it is still there.
	t.Run("rejects a revoke GitHub did not apply", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{
			ownerPages:      [][]enterpriseStubOwner{{{id: testLoginID, login: testLogin}}},
			silentMutations: true,
		}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		_, err := builder.Revoke(ctx, &v2.Grant{Principal: principal, Entitlement: ent})
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.Equal(t, enterpriseAdministratorRoleUnaffiliated, stub.updatedRole)
	})

	t.Run("reports no owner access as already revoked", func(t *testing.T) {
		t.Parallel()
		stub := &enterpriseStub{}
		builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

		annos, err := builder.Revoke(ctx, &v2.Grant{Principal: principal, Entitlement: ent})
		require.NoError(t, err)
		require.Empty(t, stub.updatedRole)
		require.Empty(t, stub.cancelledID)

		var alreadyRevoked v2.GrantAlreadyRevoked
		require.True(t, annos.Contains(&alreadyRevoked))
	})
}

// drainGrants runs the sync the way the SDK does, following the page token
// until it empties, and returns every grant across both phases.
func drainGrants(
	t *testing.T,
	builder *enterpriseRoleResourceType,
	resource *v2.Resource,
) []*v2.Grant {
	t.Helper()

	var all []*v2.Grant
	token := ""
	for calls := 0; ; calls++ {
		require.Less(t, calls, 20, "the page token never emptied")
		grants, result, err := builder.Grants(context.Background(), resource,
			resourceSdk.SyncOpAttrs{PageToken: pagination.Token{Token: token}})
		require.NoError(t, err)
		all = append(all, grants...)
		if result.NextPageToken == "" {
			return all
		}
		token = result.NextPageToken
	}
}

// principals reduces grants to the principal IDs, which is what C1 keys access
// on and therefore what these tests care about.
func principals(grants []*v2.Grant) []string {
	out := make([]string, 0, len(grants))
	for _, g := range grants {
		out = append(out, g.GetPrincipal().GetId().GetResource())
	}
	return out
}

// An invitation nobody has accepted is emitted as a grant so C1 keeps a record
// of the request. C1 has no pending state, so it is the same entitlement an
// accepted owner gets; the connector tells them apart only when revoking.
func TestEnterpriseRoleGrantsIncludePendingInvitations(t *testing.T) {
	t.Parallel()

	stub := &enterpriseStub{
		ownerPages:  [][]enterpriseStubOwner{{{id: 101, login: "accepted-owner"}}},
		memberPages: [][]enterpriseStubOwner{{{id: 202, login: "invited-member"}, {id: 303, login: "plain-member"}}},
		invitations: map[string]string{"invited-member": "EAI_invited"},
	}
	builder, _, ent := newTestEnterpriseRoleBuilder(t, stub)

	require.ElementsMatch(t, []string{"101", "202"}, principals(drainGrants(t, builder, ent.Resource)))
	// One request answered both members, rather than one lookup per login.
	require.Equal(t, 1, stub.invitationBatches)
	require.Zero(t, stub.invitationQueries, "the sync must not use the single-login lookup")
}

// GitHub stops resolving an invitation that expires or is cancelled, so it
// simply stops being emitted and C1 drops the grant. Nothing tracks an expiry.
func TestEnterpriseRoleGrantsDropInvitationsThatStoppedResolving(t *testing.T) {
	t.Parallel()

	stub := &enterpriseStub{
		memberPages: [][]enterpriseStubOwner{{{id: 202, login: "invited-member"}}},
		invitations: map[string]string{"invited-member": "EAI_invited"},
	}
	builder, _, ent := newTestEnterpriseRoleBuilder(t, stub)
	require.Equal(t, []string{"202"}, principals(drainGrants(t, builder, ent.Resource)))

	// The invitation lapses on GitHub's side.
	delete(stub.invitations, "invited-member")
	require.Empty(t, drainGrants(t, builder, ent.Resource))

	// Had the invitee accepted instead, they would surface as an owner.
	stub.ownerPages = [][]enterpriseStubOwner{{{id: 202, login: "invited-member"}}}
	require.Equal(t, []string{"202"}, principals(drainGrants(t, builder, ent.Resource)))
}

// A member who was never invited must not produce a grant, even though the
// batch reports them as NOT_FOUND alongside the invitation that did resolve.
func TestEnterpriseRoleGrantsIgnoreMembersWithoutAnInvitation(t *testing.T) {
	t.Parallel()

	stub := &enterpriseStub{
		memberPages: [][]enterpriseStubOwner{{
			{id: 202, login: "invited-member"},
			{id: 303, login: "plain-member"},
			{id: 404, login: "another-plain-member"},
		}},
		invitations: map[string]string{"invited-member": "EAI_invited"},
	}
	builder, _, ent := newTestEnterpriseRoleBuilder(t, stub)

	require.Equal(t, []string{"202"}, principals(drainGrants(t, builder, ent.Resource)))
}

// The invitation batch always reports NOT_FOUND for the members with no
// invitation, so those entries must not decide the code for the whole
// response. NOT_FOUND is the one code the SDK downgrades to a warning: if a
// real failure were reported as NOT_FOUND, the sync would finish green having
// emitted no pending invitations, and C1 would read that as a revoke.
func TestEnterpriseRoleGrantsFailOnARealErrorInsideTheInvitationBatch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		errorType string
		want      codes.Code
	}{
		{errorType: "FORBIDDEN", want: codes.PermissionDenied},
		{errorType: "UNAUTHENTICATED", want: codes.Unauthenticated},
		{errorType: "RATE_LIMITED", want: codes.Unavailable},
		// An error type the classifier has no rule for must still fail the
		// batch rather than inherit NOT_FOUND from its neighbours.
		{errorType: "SERVICE_UNAVAILABLE", want: codes.Internal},
	} {
		t.Run(tc.errorType, func(t *testing.T) {
			t.Parallel()
			stub := &enterpriseStub{
				memberPages:    [][]enterpriseStubOwner{{{id: 202, login: "plain-member"}}},
				batchErrorType: tc.errorType,
			}
			builder, _, ent := newTestEnterpriseRoleBuilder(t, stub)

			// Phase one reports no owners, then the invitation phase fails.
			_, result, err := builder.Grants(context.Background(), ent.Resource, resourceSdk.SyncOpAttrs{})
			require.NoError(t, err)
			_, _, err = builder.Grants(context.Background(), ent.Resource,
				resourceSdk.SyncOpAttrs{PageToken: pagination.Token{Token: result.NextPageToken}})
			require.Equal(t, tc.want, status.Code(err))
		})
	}
}

// The members are paged, and every page gets its own batched lookup.
func TestEnterpriseRoleGrantsPageThroughMembers(t *testing.T) {
	t.Parallel()

	stub := &enterpriseStub{
		memberPages: [][]enterpriseStubOwner{
			{{id: 202, login: "invited-member"}},
			{{id: 303, login: "second-page-invitee"}},
		},
		invitations: map[string]string{
			"invited-member":      "EAI_one",
			"second-page-invitee": "EAI_two",
		},
	}
	builder, _, ent := newTestEnterpriseRoleBuilder(t, stub)

	require.ElementsMatch(t, []string{"202", "303"}, principals(drainGrants(t, builder, ent.Resource)))
	require.Equal(t, 2, stub.memberQueries)
	require.Equal(t, 2, stub.invitationBatches)
}

// The owners are read through the organization, so an organization that
// belongs to a different enterprise would report the wrong owners.
func TestEnterpriseRoleVerifyOrganization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stub := &enterpriseStub{}
	builder, _, _ := newTestEnterpriseRoleBuilder(t, stub)
	enterpriseClients, err := builder.clients(ctx)
	require.NoError(t, err)
	client := enterpriseClients[testEnterprise]

	require.NoError(t, client.verifyOrganization(ctx, testEnterprise))
	require.Equal(t, 1, stub.organizationChecks)

	client.org = "org-of-another-enterprise"
	err = client.verifyOrganization(ctx, testEnterprise)
	require.ErrorContains(t, err, "does not belong to enterprise")
}

// A missing enterprise installation must fail this resource type and nothing
// else, so the rest of the connector still syncs. Returning no resources would
// read to C1 as a revoke of every owner assignment.
func TestEnterpriseRoleFailsClosedWithoutEnterpriseClients(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clientsErr := errors.New("github-connector: GitHub App is not installed on enterprise")
	builds := 0
	builder := EnterpriseRoleBuilder(nil, nil, nil, []string{testEnterprise},
		func(context.Context) (map[string]*githubEnterpriseAdministratorClient, error) {
			builds++
			return nil, clientsErr
		},
	)

	_, _, err := builder.List(ctx, nil, resourceSdk.SyncOpAttrs{})
	require.ErrorIs(t, err, clientsErr)

	roleResource, err := resourceSdk.NewRoleResource(
		enterpriseRoleOwner,
		resourceTypeEnterpriseRole,
		testEnterprise+":"+enterpriseRoleOwner,
		[]resourceSdk.RoleTraitOption{},
	)
	require.NoError(t, err)

	_, _, err = builder.Grants(ctx, roleResource, resourceSdk.SyncOpAttrs{})
	require.ErrorIs(t, err, clientsErr)

	// A misconfiguration answers the same way every time, so it is built once
	// and remembered rather than re-probed on each call.
	require.Equal(t, 1, builds)
}

// A transient failure must not be remembered: freezing a blip at startup would
// disable the resource type until the process restarts.
func TestEnterpriseRoleRetriesARetryableClientFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	builds := 0
	builder := EnterpriseRoleBuilder(nil, nil, nil, []string{testEnterprise},
		func(context.Context) (map[string]*githubEnterpriseAdministratorClient, error) {
			builds++
			if builds == 1 {
				return nil, status.Error(codes.Unavailable, "rate limited")
			}
			// List only checks the enterprise is present, so the client
			// itself is never dereferenced here.
			return map[string]*githubEnterpriseAdministratorClient{testEnterprise: nil}, nil
		},
	)

	_, _, err := builder.List(ctx, nil, resourceSdk.SyncOpAttrs{})
	require.Equal(t, codes.Unavailable, status.Code(err))

	resources, _, err := builder.List(ctx, nil, resourceSdk.SyncOpAttrs{})
	require.NoError(t, err)
	require.Len(t, resources, 1)
	require.Equal(t, 2, builds)

	// Once it succeeds the result is memoized.
	_, _, err = builder.List(ctx, nil, resourceSdk.SyncOpAttrs{})
	require.NoError(t, err)
	require.Equal(t, 2, builds)
}

// Provisioning is only possible through an enterprise installation, so the PAT
// path must reject it rather than attempt a mutation it cannot make.
func TestEnterpriseRoleProvisioningTargetGuards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stub := &enterpriseStub{}
	builder, principal, ent := newTestEnterpriseRoleBuilder(t, stub)

	t.Run("rejects a principal that is not a user", func(t *testing.T) {
		t.Parallel()
		notAUser := &v2.Resource{Id: &v2.ResourceId{
			ResourceType: resourceTypeTeam.Id,
			Resource:     "1",
		}}
		_, _, err := builder.Grant(ctx, notAUser, ent)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("rejects an enterprise without an app installation", func(t *testing.T) {
		t.Parallel()
		patBuilder := EnterpriseRoleBuilder(nil, nil, nil, []string{testEnterprise}, nil)
		_, _, err := patBuilder.Grant(ctx, principal, ent)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
}

// An enterprise owner does not have to be an owner of the organization the app
// reads them through: observed live with a user whose organizationRole was
// DIRECT_MEMBER. Organization.enterpriseOwners returns them regardless, and the
// connector must emit their grant.
// GitHub reports a GraphQL budget error as an HTTP 200 carrying errors[], so
// the status of the response says nothing. Reading the owners is the hottest
// GraphQL path in this role, and an unclassified budget error reaches the SDK
// as Unknown, which it does not retry: the sync aborts instead of backing off.
func TestEnterpriseRoleGrantsClassifyARateLimitedOwnersRead(t *testing.T) {
	t.Parallel()

	builder, _, ent := newTestEnterpriseRoleBuilder(t, &enterpriseStub{ownersRateLimited: true})

	_, _, err := builder.Grants(context.Background(), ent.Resource, resourceSdk.SyncOpAttrs{})
	require.Equal(t, codes.Unavailable, status.Code(err))
}

func TestEnterpriseRoleGrantsIncludeOwnerWhoIsNotAnOrgOwner(t *testing.T) {
	t.Parallel()

	stub := &enterpriseStub{
		ownerPages: [][]enterpriseStubOwner{{
			{id: 158784853, login: "org-owner"},
			{id: 162376288, login: "enterprise-owner-only"},
		}},
	}
	builder, _, ent := newTestEnterpriseRoleBuilder(t, stub)

	require.ElementsMatch(t, []string{"158784853", "162376288"},
		principals(drainGrants(t, builder, ent.Resource)))
}

func TestEnterpriseRoleGrantsPagination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stub := &enterpriseStub{
		ownerPages: [][]enterpriseStubOwner{
			{{id: 101, login: "owner-page-one"}},
			{{id: 102, login: "owner-page-two"}},
		},
	}
	builder, _, ent := newTestEnterpriseRoleBuilder(t, stub)

	resources, _, err := builder.List(ctx, nil, resourceSdk.SyncOpAttrs{})
	require.NoError(t, err)
	require.Len(t, resources, 1)
	require.Equal(t, testEnterprise+":"+enterpriseRoleOwner, resources[0].GetId().GetResource())
	require.Zero(t, stub.ownerQueries)

	grants, result, err := builder.Grants(ctx, ent.Resource, resourceSdk.SyncOpAttrs{})
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, "101", grants[0].GetPrincipal().GetId().GetResource())
	require.NotEmpty(t, result.NextPageToken)
	require.NotEmpty(t, result.Annotations)

	// The SDK drives the second page with the cursor from the first.
	grants, result, err = builder.Grants(ctx, ent.Resource, resourceSdk.SyncOpAttrs{
		PageToken: pagination.Token{Token: result.NextPageToken},
	})
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, "102", grants[0].GetPrincipal().GetId().GetResource())
	require.Equal(t, 2, stub.ownerQueries)

	// The owners are exhausted, so the token moves on to the invitations
	// rather than ending the sync.
	require.NotEmpty(t, result.NextPageToken)
	grants, result, err = builder.Grants(ctx, ent.Resource, resourceSdk.SyncOpAttrs{
		PageToken: pagination.Token{Token: result.NextPageToken},
	})
	require.NoError(t, err)
	require.Empty(t, grants)
	require.Empty(t, result.NextPageToken)
	require.Equal(t, 2, stub.ownerQueries, "the invitation phase must not re-read the owners")
	require.Equal(t, 1, stub.memberQueries)
}
