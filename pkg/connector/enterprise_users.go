package connector

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/conductorone/baton-github/pkg/customclient"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/shurcooL/githubv4"
	"go.uber.org/zap"
)

const (
	// Phases of the user list that run after an organization's members, one
	// page per call. They emit the principals enterprise-role grants name.
	enterpriseUserOwnersPhase   = "enterprise-user-owners"
	enterpriseUserPendingPhase  = "enterprise-user-pending"
	enterpriseUserLicensesPhase = "enterprise-user-licenses"
)

// tracksEnterprisePrincipals reports whether this sync resolves the principals
// of enterprise-role grants. Without the role type there are no such grants.
func (u *userResourceType) tracksEnterprisePrincipals() bool {
	return len(u.enterprises) > 0 && u.syncsEnterpriseRoles
}

func isEnterpriseUserPhase(phase string) bool {
	switch phase {
	case enterpriseUserOwnersPhase, enterpriseUserPendingPhase, enterpriseUserLicensesPhase:
		return true
	default:
		return false
	}
}

// nextUserPageToken advances the organization's member pages, and on the last
// one hands the page token to the enterprise-principal phases.
//
// Queuing those phases reads the enterprise, so this page can fail on one that
// is unreadable. NotFound has to be converted: the SDK treats it as a warning
// and completes the sync, dropping the members this page already fetched.
func (u *userResourceType) nextUserPageToken(
	ctx context.Context,
	bag *pagination.Bag,
	nextPage string,
) (string, error) {
	if nextPage == "" {
		token, queued, err := u.continueWithEnterprisePrincipals(ctx, bag)
		if err != nil {
			return "", failClosedOnUnreadableEnterprise(err, strings.Join(u.enterprises, ", "))
		}
		if queued {
			return token, nil
		}
	}

	return bag.NextToken(nextPage)
}

// continueWithEnterprisePrincipals appends the enterprise-principal phases to
// bag. It returns the next page token and true when the member page should
// continue into those phases.
//
// App authentication walks Owners and pending Owner invitees, the two sets
// Grants emits; a personal access token walks the consumed-licenses users that
// hold an enterprise role. A setup error skips the phases, because the role
// type skips its grants for the same reason.
func (u *userResourceType) continueWithEnterprisePrincipals(
	ctx context.Context,
	bag *pagination.Bag,
) (string, bool, error) {
	if !u.tracksEnterprisePrincipals() {
		return "", false, nil
	}

	enterprises := distinctEnterprises(u.enterprises)
	phases := []string{enterpriseUserLicensesPhase}
	slugs := enterprises
	if u.newEnterpriseClients != nil {
		clients, err := u.newEnterpriseClients(ctx, enterprises)
		if err != nil {
			if isEnterpriseSetupError(err) {
				ctxzap.Extract(ctx).Debug(
					"baton-github: not syncing enterprise principals as users, the GitHub App is not set up to read them",
					zap.Error(err),
				)
				return "", false, nil
			}
			return "", false, err
		}
		phases = []string{enterpriseUserOwnersPhase, enterpriseUserPendingPhase}
		slugs = enterprisesWithClients(slugs, clients)
	}
	plan := enterpriseUserPhasePlan(slugs, phases)
	if len(plan) == 0 {
		return "", false, nil
	}

	queueEnterpriseUserPhases(bag, plan)
	token, err := bag.Marshal()
	if err != nil {
		return "", false, fmt.Errorf("baton-github: error encoding the enterprise user page token: %w", err)
	}

	return token, true, nil
}

// enterpriseUserPhasePlan lists the pages the walk visits, in the order it
// visits them: every phase of one enterprise before moving to the next.
func enterpriseUserPhasePlan(slugs []string, phases []string) []pagination.PageState {
	plan := make([]pagination.PageState, 0, len(slugs)*len(phases))
	for _, slug := range slugs {
		for _, phase := range phases {
			plan = append(plan, pagination.PageState{
				ResourceTypeID: phase,
				ResourceID:     slug,
			})
		}
	}

	return plan
}

// queueEnterpriseUserPhases replaces the finished member state with the plan.
// The bag is a stack, so the plan is pushed back to front and its first page
// ends up on top.
func queueEnterpriseUserPhases(bag *pagination.Bag, plan []pagination.PageState) {
	bag.Pop()
	for _, state := range slices.Backward(plan) {
		bag.Push(state)
	}
}

func enterprisesWithClients(slugs []string, clients map[string]*customclient.EnterpriseAdminClient) []string {
	var withClients []string
	for _, enterprise := range slugs {
		if _, ok := clients[enterprise]; ok {
			withClients = append(withClients, enterprise)
		}
	}

	return withClients
}

// listEnterprisePrincipalUsers emits one page of enterprise principals whose
// user resource the organization member list does not produce.
func (u *userResourceType) listEnterprisePrincipalUsers(
	ctx context.Context,
	bag *pagination.Bag,
) ([]*v2.Resource, *resource.SyncOpResults, error) {
	// Resolved once for the page: every principal on it is checked against the
	// same organizations.
	orgs, annos, err := u.syncedOrgs(ctx)
	if err != nil {
		return nil, &resource.SyncOpResults{Annotations: annos}, err
	}

	var (
		ret       []*v2.Resource
		next      string
		pageAnnos annotations.Annotations
	)
	switch phase := bag.ResourceTypeID(); phase {
	case enterpriseUserOwnersPhase:
		ret, next, pageAnnos, err = u.listOwnerUsers(ctx, orgs, bag.ResourceID(), bag.PageToken())
	case enterpriseUserPendingPhase:
		ret, next, pageAnnos, err = u.listPendingOwnerUsers(ctx, orgs, bag.ResourceID(), bag.PageToken())
	case enterpriseUserLicensesPhase:
		ret, next, pageAnnos, err = u.listLicensedRoleUsers(ctx, orgs, bag.ResourceID(), bag.PageToken())
	default:
		return nil, nil, fmt.Errorf("baton-github: unexpected user sync phase %q", phase)
	}
	annos = freshestRateLimit(annos, pageAnnos)
	// As in nextUserPageToken: a NotFound would be a warning, and the sync would
	// report no enterprise principals while the role syncer emits their grants.
	if err != nil {
		return nil, &resource.SyncOpResults{Annotations: annos},
			failClosedOnUnreadableEnterprise(err, bag.ResourceID())
	}

	token, err := bag.NextToken(next)
	if err != nil {
		return nil, &resource.SyncOpResults{Annotations: annos},
			fmt.Errorf("baton-github: error advancing the enterprise user page token: %w", err)
	}

	return ret, &resource.SyncOpResults{
		NextPageToken: token,
		Annotations:   annos,
	}, nil
}

func (u *userResourceType) listOwnerUsers(
	ctx context.Context,
	orgs []string,
	enterprise string,
	cursor string,
) ([]*v2.Resource, string, annotations.Annotations, error) {
	client, err := u.adminClient(ctx, enterprise)
	if err != nil {
		return nil, "", nil, err
	}

	owners, next, annos, err := client.Owners(ctx, graphqlCursor(cursor))
	if err != nil {
		return nil, "", annos, err
	}

	ret, annos, err := u.resourcesForEnterpriseUsers(ctx, orgs, owners, annos)
	if err != nil {
		return nil, "", annos, err
	}

	return ret, next, annos, nil
}

func (u *userResourceType) listPendingOwnerUsers(
	ctx context.Context,
	orgs []string,
	enterprise string,
	cursor string,
) ([]*v2.Resource, string, annotations.Annotations, error) {
	client, err := u.adminClient(ctx, enterprise)
	if err != nil {
		return nil, "", nil, err
	}

	members, next, annos, err := client.Members(ctx, enterprise, graphqlCursor(cursor))
	if err != nil {
		return nil, "", annos, err
	}
	if len(members) == 0 {
		return nil, next, annos, nil
	}

	logins := make([]string, 0, len(members))
	for _, member := range members {
		logins = append(logins, member.Login)
	}
	invitations, invitationAnnos, err := client.PendingOwnerInvitations(ctx, enterprise, logins)
	annos = freshestRateLimit(annos, invitationAnnos)
	if err != nil {
		return nil, "", annos, err
	}

	invited := make([]customclient.EnterpriseUser, 0, len(invitations))
	for _, member := range members {
		if _, ok := invitations[member.Login]; ok {
			invited = append(invited, member)
		}
	}

	ret, annos, err := u.resourcesForEnterpriseUsers(ctx, orgs, invited, annos)
	if err != nil {
		return nil, "", annos, err
	}

	return ret, next, annos, nil
}

// listLicensedRoleUsers emits the consumed-licenses users who hold an
// enterprise role, which is the set the token path's Grants emits.
func (u *userResourceType) listLicensedRoleUsers(
	ctx context.Context,
	orgs []string,
	enterprise string,
	cursor string,
) ([]*v2.Resource, string, annotations.Annotations, error) {
	page := 1
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, "", nil, fmt.Errorf("baton-github: invalid consumed-licenses page %q: %w", cursor, err)
		}
		page = parsed
	}

	var annos annotations.Annotations
	consumed, rateLimit, err := u.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise, page)
	annos.WithRateLimiting(rateLimit)
	if err != nil {
		return nil, "", annos, fmt.Errorf(
			"baton-github: error listing enterprise consumed licenses for %s: %w", enterprise, err)
	}
	if len(consumed.Users) == 0 {
		return nil, "", annos, nil
	}

	ret := make([]*v2.Resource, 0, len(consumed.Users))
	for _, licensed := range consumed.Users {
		if licensed.GitHubComLogin == "" || len(licensed.GitHubComEnterpriseRoles) == 0 {
			continue
		}
		emitted, memberAnnos, err := u.emittedByOrgMembers(ctx, orgs, licensed.GitHubComLogin)
		annos = freshestRateLimit(annos, memberAnnos)
		if err != nil {
			return nil, "", annos, err
		}
		if emitted {
			continue
		}

		ghUser, resp, err := u.client.Users.Get(ctx, licensed.GitHubComLogin)
		if restRateLimit, rateLimitErr := extractRateLimitData(resp); rateLimitErr == nil {
			annos.WithRateLimiting(restRateLimit)
		}
		if err != nil {
			return nil, "", annos, wrapGitHubError(err, resp,
				fmt.Sprintf("baton-github: failed to get user %s", licensed.GitHubComLogin))
		}
		ur, err := userResource(ctx, ghUser, ghUser.GetEmail(), nil)
		if err != nil {
			return nil, "", annos, err
		}
		ret = append(ret, ur)
	}

	return ret, strconv.Itoa(page + 1), annos, nil
}

// adminClient builds the administration client of one enterprise. Each page
// builds its own, since this syncer keeps no state between pages.
func (u *userResourceType) adminClient(
	ctx context.Context,
	enterprise string,
) (*customclient.EnterpriseAdminClient, error) {
	clients, err := u.newEnterpriseClients(ctx, []string{enterprise})
	if err != nil {
		return nil, err
	}
	client, ok := clients[enterprise]
	if !ok {
		return nil, fmt.Errorf("baton-github: enterprise %s has no administration client", enterprise)
	}

	return client, nil
}

// resourcesForEnterpriseUsers builds a user resource for each principal no
// organization in this sync emits.
func (u *userResourceType) resourcesForEnterpriseUsers(
	ctx context.Context,
	orgs []string,
	users []customclient.EnterpriseUser,
	annos annotations.Annotations,
) ([]*v2.Resource, annotations.Annotations, error) {
	ret := make([]*v2.Resource, 0, len(users))
	for _, user := range users {
		if user.DatabaseID <= 0 {
			ctxzap.Extract(ctx).Debug("baton-github: skipping an enterprise principal with no database ID",
				zap.String("login", user.Login),
			)
			continue
		}

		emitted, memberAnnos, err := u.emittedByOrgMembers(ctx, orgs, user.Login)
		annos = freshestRateLimit(annos, memberAnnos)
		if err != nil {
			return nil, annos, err
		}
		if emitted {
			continue
		}

		ur, userAnnos, err := u.userFromDatabaseID(ctx, user.DatabaseID, user.Login)
		annos = freshestRateLimit(annos, userAnnos)
		if err != nil {
			return nil, annos, err
		}
		ret = append(ret, ur)
	}

	return ret, annos, nil
}

// emittedByOrgMembers reports whether an organization this sync walks already
// emits the login. Those member pages carry the user's SAML identity, and the
// resource store keeps the last write, so emitting the account again here
// would drop it.
func (u *userResourceType) emittedByOrgMembers(
	ctx context.Context,
	orgs []string,
	login string,
) (bool, annotations.Annotations, error) {
	var annos annotations.Annotations
	for _, org := range orgs {
		member, resp, err := u.client.Organizations.IsMember(ctx, org, login)
		if rateLimit, rateLimitErr := extractRateLimitData(resp); rateLimitErr == nil {
			annos.WithRateLimiting(rateLimit)
		}
		if err != nil {
			return false, annos, wrapGitHubError(err, resp,
				fmt.Sprintf("baton-github: failed to check whether %s belongs to organization %s", login, org))
		}
		if member {
			return true, annos, nil
		}
	}

	return false, annos, nil
}

// syncedOrgs returns the organizations whose member pages this sync emits. The
// set has to match what the organization syncer produces: counting a principal
// as emitted by an organization this sync leaves out would drop the user and
// leave the grant pointing at nothing.
//
// A named organization is taken as given, because the organization syncer does
// the same: under a GitHub App the installation decides, and under a token
// validation already refused a credential that does not administer one. Naming
// none syncs every organization the credential administers, so that filter has
// to run here too.
func (u *userResourceType) syncedOrgs(ctx context.Context) ([]string, annotations.Annotations, error) {
	var annos annotations.Annotations
	if len(u.orgs) > 0 {
		return u.orgs, annos, nil
	}

	candidates, err := getOrgs(ctx, u.client, nil)
	if err != nil {
		return nil, annos, err
	}

	administered := make([]string, 0, len(candidates))
	for _, org := range candidates {
		isAdmin, resp, err := administersOrg(ctx, u.client, org)
		if rateLimit, rateLimitErr := extractRateLimitData(resp); rateLimitErr == nil {
			annos.WithRateLimiting(rateLimit)
		}
		if err != nil {
			return nil, annos, wrapGitHubError(err, resp,
				fmt.Sprintf("baton-github: failed to read the membership of organization %s", org))
		}
		if isAdmin {
			administered = append(administered, org)
		}
	}

	return administered, annos, nil
}

// userFromDatabaseID reads the account behind an enterprise principal. A 404
// still emits the principal from the database ID and login the enterprise query
// returned: dropping it would leave the grant pointing at nothing.
func (u *userResourceType) userFromDatabaseID(
	ctx context.Context,
	id int64,
	login string,
) (*v2.Resource, annotations.Annotations, error) {
	var annos annotations.Annotations
	ghUser, resp, err := u.client.Users.GetByID(ctx, id)
	if rateLimit, rateLimitErr := extractRateLimitData(resp); rateLimitErr == nil {
		annos.WithRateLimiting(rateLimit)
	}
	if err != nil {
		if !isNotFoundError(resp) {
			return nil, annos, wrapGitHubError(err, resp, fmt.Sprintf("baton-github: failed to get user %d", id))
		}
		ctxzap.Extract(ctx).Debug("baton-github: emitting an enterprise principal whose account did not resolve",
			zap.Int64("user_id", id),
			zap.String("login", login),
		)
		ghUser = &github.User{ID: &id, Login: &login}
	}

	ur, err := userResource(ctx, ghUser, ghUser.GetEmail(), nil)
	if err != nil {
		return nil, annos, err
	}

	return ur, annos, nil
}

func graphqlCursor(cursor string) *githubv4.String {
	if cursor == "" {
		return nil
	}

	return githubv4.NewString(githubv4.String(cursor))
}
