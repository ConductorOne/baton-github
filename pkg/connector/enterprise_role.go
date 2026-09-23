package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/conductorone/baton-github/pkg/customclient"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/shurcooL/githubv4"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	enterpriseRoleAssigned = "assigned"
	enterpriseRoleOwner    = "Owner"

	// Sync phases of the Owner grants. The invitations cannot be listed, so
	// they are resolved from the members in a second pass over the same token.
	enterpriseOwnersPhase  = "enterprise-owners"
	enterprisePendingPhase = "enterprise-pending-invitations"

	// UNAFFILIATED demotes an administrator while keeping their enterprise
	// membership; removeEnterpriseAdmin would evict them from the enterprise.
	enterpriseAdministratorRoleUnaffiliated githubv4.EnterpriseAdministratorRole = "UNAFFILIATED"
)

// enterpriseClientProvider builds the per-enterprise administration clients.
type enterpriseClientProvider func(ctx context.Context) (map[string]*githubEnterpriseAdministratorClient, error)

type enterpriseRoleResourceType struct {
	resourceType   *v2.ResourceType
	client         *github.Client
	appClient      *github.Client
	customClient   *customclient.Client
	enterprises    []string
	roleUsersCache map[string][]string
	mu             *sync.Mutex
	// newEnterpriseClients builds the per-enterprise administration clients.
	// It is nil under PAT auth.
	newEnterpriseClients enterpriseClientProvider
	// enterpriseClients is keyed by enterprise slug and memoized after the
	// first successful build. A build error is returned rather than stored,
	// so the sync fails instead of completing without owners and the next
	// call tries again.
	enterpriseClients map[string]*githubEnterpriseAdministratorClient
}

func (o *enterpriseRoleResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

// clients returns the per-enterprise administration clients, building them on
// first use and memoizing them once they are built.
//
// Only success is remembered. A failure is retried on the next call, which
// costs one discovery attempt on a path that is already failing the sync, and
// buys two things: a startup blip does not disable this resource type for the
// lifetime of the process, and an operator who installs the app or restores
// the permission is picked up by the next sync instead of needing a restart.
// GitHub answers 404 for an uninstalled app, a revoked permission and a typo
// alike, so no error here can be trusted to be permanent.
func (o *enterpriseRoleResourceType) clients(
	ctx context.Context,
) (map[string]*githubEnterpriseAdministratorClient, error) {
	if o.newEnterpriseClients == nil {
		return nil, nil
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if o.enterpriseClients != nil {
		return o.enterpriseClients, nil
	}

	clients, err := o.newEnterpriseClients(ctx)
	if err != nil {
		return nil, err
	}
	o.enterpriseClients = clients

	return o.enterpriseClients, nil
}

// noClientReason explains why no administration client exists, in the terms of
// the fix the operator has to make. Under app auth every reason already
// surfaced as a build error, so what is left is the credential and an
// enterprise this connector was not configured for.
func (o *enterpriseRoleResourceType) noClientReason() string {
	if o.newEnterpriseClients == nil {
		return "a personal access token can sync enterprise roles but cannot provision them, " +
			"which needs GitHub App authentication"
	}
	return "it is not one of the configured enterprises"
}

func (o *enterpriseRoleResourceType) cacheRole(roleId string, userLogin string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.roleUsersCache[roleId]; !exists {
		o.roleUsersCache[roleId] = []string{}
	}

	o.roleUsersCache[roleId] = append(o.roleUsersCache[roleId], userLogin)
}

func (o *enterpriseRoleResourceType) getRoleUsersCache(ctx context.Context) (map[string][]string, error) {
	if len(o.roleUsersCache) == 0 {
		if err := o.fillCache(ctx); err != nil {
			return nil, fmt.Errorf("baton-github: error caching user roles: %w", err)
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	return o.roleUsersCache, nil
}

func (o *enterpriseRoleResourceType) fillCache(ctx context.Context) error {
	l := ctxzap.Extract(ctx)
	for _, enterprise := range o.enterprises {
		// GitHub's consumed-licenses API is 1-indexed; page 0 is undocumented
		// and may return the same results as page 1, causing duplicates.
		page := 1
		continuePagination := true
		for continuePagination {
			consumedLicenses, _, err := o.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise, page)
			if err != nil {
				if page == 1 && o.appClient != nil && isPermissionDenied(err) {
					l.Debug("baton-github: enterprise features (--enterprises) require a Personal Access Token. "+
						"GitHub App authentication cannot access the consumed-licenses API. "+
						"Either switch to PAT auth or remove the --enterprises flag.",
						zap.String("enterprise", enterprise),
						zap.Error(err))
					return nil
				}
				return fmt.Errorf("baton-github: error listing enterprise consumed licenses for %s: %w", enterprise, err)
			}

			if len(consumedLicenses.Users) == 0 {
				continuePagination = false
			}
			page++

			for _, user := range consumedLicenses.Users {
				for _, role := range user.GitHubComEnterpriseRoles {
					roleId := fmt.Sprintf("%s:%s", enterprise, role)
					o.cacheRole(roleId, user.GitHubComLogin)
				}
			}
		}
	}
	return nil
}

func (o *enterpriseRoleResourceType) List(
	ctx context.Context,
	parentID *v2.ResourceId,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	enterpriseClients, err := o.clients(ctx)
	if err != nil {
		return nil, nil, err
	}

	// The consumed-licenses API that backs the cache is PAT-only, so a GitHub
	// App can only see the built-in Owner role it is able to read and mutate.
	if len(enterpriseClients) > 0 {
		return appList(o.enterprises, enterpriseClients)
	}

	var ret []*v2.Resource
	cache, err := o.getRoleUsersCache(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-github: error getting user roles cache: %w", err)
	}

	for roleId := range cache {
		roleName := strings.Split(roleId, ":")[1]
		enterprise := strings.Split(roleId, ":")[0]

		roleResource, err := resourceSdk.NewRoleResource(
			roleName,
			resourceTypeEnterpriseRole,
			roleId,
			[]resourceSdk.RoleTraitOption{},
		)
		if err != nil {
			return nil, nil, fmt.Errorf("baton-github: error creating role resource for %s in enterprise %s: %w", roleName, enterprise, err)
		}
		ret = append(ret, roleResource)
	}

	return ret, &resourceSdk.SyncOpResults{}, nil
}

func (o *enterpriseRoleResourceType) Entitlements(
	_ context.Context,
	resource *v2.Resource,
	_ resourceSdk.SyncOpAttrs,
) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	return nil, nil, nil
}

func (o *enterpriseRoleResourceType) StaticEntitlements(
	_ context.Context,
	_ resourceSdk.SyncOpAttrs,
) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	rv := []*v2.Entitlement{}
	rv = append(rv, entitlement.NewAssignmentEntitlement(nil, enterpriseRoleAssigned,
		entitlement.WithDisplayName("Role Assigned"),
		entitlement.WithDescription("Assignment to enterprise role in GitHub"),
		entitlement.WithGrantableTo(resourceTypeUser),
	))

	return rv, &resourceSdk.SyncOpResults{}, nil
}

func (o *enterpriseRoleResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	enterpriseClients, err := o.clients(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(enterpriseClients) > 0 {
		return o.appGrants(ctx, enterpriseClients, resource, opts)
	}

	cache, err := o.getRoleUsersCache(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-github: error getting user roles cache: %w", err)
	}

	ret := []*v2.Grant{}
	for _, userLogin := range cache[resource.Id.Resource] {
		user, resp, err := o.client.Users.Get(ctx, userLogin)
		if err != nil {
			return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("baton-github: failed to get user %s", userLogin))
		}

		principalId, err := resourceSdk.NewResourceID(resourceTypeUser, *user.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("baton-github: error creating resource ID for user %s: %w", userLogin, err)
		}

		ret = append(ret, grant.NewGrant(
			resource,
			enterpriseRoleAssigned,
			principalId,
		))
	}

	return ret, &resourceSdk.SyncOpResults{}, nil
}

// EnterpriseRoleBuilder returns the enterprise role syncer. newEnterpriseClients
// is nil under PAT authentication, where only the read path is available.
func EnterpriseRoleBuilder(
	client *github.Client,
	appClient *github.Client,
	customClient *customclient.Client,
	enterprises []string,
	newEnterpriseClients enterpriseClientProvider,
) *enterpriseRoleResourceType {
	return &enterpriseRoleResourceType{
		resourceType:         resourceTypeEnterpriseRole,
		client:               client,
		appClient:            appClient,
		customClient:         customClient,
		enterprises:          enterprises,
		roleUsersCache:       make(map[string][]string),
		mu:                   &sync.Mutex{},
		newEnterpriseClients: newEnterpriseClients,
	}
}

// appList emits only the built-in Owner role. The consumed-licenses API that
// discovers the other roles is PAT-only.
func appList(
	enterprises []string,
	enterpriseClients map[string]*githubEnterpriseAdministratorClient,
) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	var ret []*v2.Resource
	for _, enterprise := range enterprises {
		if _, ok := enterpriseClients[enterprise]; !ok {
			continue
		}

		roleResource, err := resourceSdk.NewRoleResource(
			enterpriseRoleOwner,
			resourceTypeEnterpriseRole,
			fmt.Sprintf("%s:%s", enterprise, enterpriseRoleOwner),
			[]resourceSdk.RoleTraitOption{},
		)
		if err != nil {
			return nil, nil, fmt.Errorf("baton-github: error creating role resource for %s in enterprise %s: %w",
				enterpriseRoleOwner, enterprise, err)
		}
		ret = append(ret, roleResource)
	}

	return ret, &resourceSdk.SyncOpResults{}, nil
}

// appGrants emits the users who hold the Owner role today and the ones invited
// but not yet accepted, against the same entitlement. C1 has no pending grant
// state, so the two are indistinguishable there; the connector still tells them
// apart, because revoking an owner and cancelling an invitation are different
// mutations.
//
// Nothing tracks an expiry: an invitation nobody accepts stops resolving on
// GitHub's side, so it stops being emitted and C1 drops the grant that sync.
//
// Owners and invitations are two phases of one page token, because invitations
// are not enumerable and have to be resolved from the enterprise members. An
// empty cursor drops the current phase, emptying the token once both are done.
func (o *enterpriseRoleResourceType) appGrants(
	ctx context.Context,
	enterpriseClients map[string]*githubEnterpriseAdministratorClient,
	resource *v2.Resource,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	enterprise, ok := provisionableEnterpriseOwner(resource.Id.Resource)
	if !ok {
		return nil, &resourceSdk.SyncOpResults{}, nil
	}
	client, ok := enterpriseClients[enterprise]
	if !ok {
		return nil, &resourceSdk.SyncOpResults{}, nil
	}

	bag := &pagination.Bag{}
	if err := bag.Unmarshal(opts.PageToken.Token); err != nil {
		return nil, nil, fmt.Errorf("baton-github: error parsing enterprise owner page token: %w", err)
	}
	if bag.Current() == nil {
		// Reverse order: the owners are walked first.
		bag.Push(pagination.PageState{ResourceTypeID: enterprisePendingPhase})
		bag.Push(pagination.PageState{ResourceTypeID: enterpriseOwnersPhase})
	}

	var after *githubv4.String
	if cursor := bag.PageToken(); cursor != "" {
		after = githubv4.NewString(githubv4.String(cursor))
	}

	var (
		ret        []*v2.Grant
		nextCursor string
		annos      annotations.Annotations
		err        error
	)
	switch phase := bag.ResourceTypeID(); phase {
	case enterpriseOwnersPhase:
		ret, nextCursor, annos, err = o.ownerGrants(ctx, client, resource, after)
	case enterprisePendingPhase:
		ret, nextCursor, annos, err = o.pendingInvitationGrants(ctx, client, resource, enterprise, after)
	default:
		return nil, nil, fmt.Errorf("baton-github: unexpected enterprise owner sync phase %q", phase)
	}
	if err != nil {
		return nil, &resourceSdk.SyncOpResults{Annotations: annos}, err
	}

	if err := bag.Next(nextCursor); err != nil {
		return nil, &resourceSdk.SyncOpResults{Annotations: annos},
			fmt.Errorf("baton-github: error advancing the enterprise owner page token: %w", err)
	}
	pageToken, err := bag.Marshal()
	if err != nil {
		return nil, &resourceSdk.SyncOpResults{Annotations: annos},
			fmt.Errorf("baton-github: error building the enterprise owner page token: %w", err)
	}

	return ret, &resourceSdk.SyncOpResults{Annotations: annos, NextPageToken: pageToken}, nil
}

// ownerGrants emits one page of the users who hold the role today.
func (o *enterpriseRoleResourceType) ownerGrants(
	ctx context.Context,
	client *githubEnterpriseAdministratorClient,
	resource *v2.Resource,
	after *githubv4.String,
) ([]*v2.Grant, string, annotations.Annotations, error) {
	owners, nextCursor, annos, err := client.owners(ctx, after)
	if err != nil {
		return nil, "", annos, err
	}

	ret := make([]*v2.Grant, 0, len(owners))
	for _, owner := range owners {
		principalId, err := enterpriseOwnerPrincipalID(owner)
		if err != nil {
			return nil, "", annos, err
		}
		ret = append(ret, grant.NewGrant(resource, enterpriseRoleAssigned, principalId))
	}

	return ret, nextCursor, annos, nil
}

// pendingInvitationGrants emits one page worth of users who have been invited
// to the role and have not accepted, in member order so a page emits the same
// grants in the same sequence on every sync.
//
// GitHub exposes no connection of pending administrator invitations to an
// installation token, so they cannot be listed: they are resolved by asking
// about known logins. The enterprise members are that candidate set, which
// means an invitation sent to someone who is not a member of the enterprise is
// invisible to the sync.
func (o *enterpriseRoleResourceType) pendingInvitationGrants(
	ctx context.Context,
	client *githubEnterpriseAdministratorClient,
	resource *v2.Resource,
	enterprise string,
	after *githubv4.String,
) ([]*v2.Grant, string, annotations.Annotations, error) {
	members, nextCursor, annos, err := client.members(ctx, enterprise, after)
	if err != nil {
		return nil, "", annos, err
	}
	if len(members) == 0 {
		return nil, nextCursor, annos, nil
	}

	logins := make([]string, 0, len(members))
	for _, member := range members {
		logins = append(logins, member.login)
	}

	invitations, invitationAnnos, err := client.pendingOwnerInvitations(ctx, enterprise, logins)
	annos = freshestRateLimit(annos, invitationAnnos)
	if err != nil {
		return nil, "", annos, err
	}

	ret := make([]*v2.Grant, 0, len(invitations))
	for _, member := range members {
		if _, invited := invitations[member.login]; !invited {
			continue
		}
		principalId, err := enterpriseOwnerPrincipalID(member)
		if err != nil {
			return nil, "", annos, err
		}
		ret = append(ret, grant.NewGrant(resource, enterpriseRoleAssigned, principalId))
	}

	return ret, nextCursor, annos, nil
}

// Grant gives a user the built-in Owner role and returns the resulting grant.
//
// A member can only become an owner by accepting an invitation, while someone
// who already administers the enterprise is promoted in place. Which case
// applies is unreadable for an installation token, so the invitation is tried
// first and the promotion is the fallback, keyed on the FailedPrecondition
// that GitHub's UNPROCESSABLE maps to.
//
// An unaccepted invitation counts as held and returns a grant, the same way
// Grants() emits it: returning nothing would make C1 drop an overlay that the
// next sync puts straight back.
func (o *enterpriseRoleResourceType) Grant(
	ctx context.Context,
	principal *v2.Resource,
	ent *v2.Entitlement,
) ([]*v2.Grant, annotations.Annotations, error) {
	enterprise, client, err := o.provisioningTarget(ctx, principal, ent)
	if err != nil {
		return nil, nil, err
	}
	login, err := o.userLogin(ctx, principal.Id.Resource)
	if err != nil {
		return nil, nil, err
	}

	result := []*v2.Grant{grant.NewGrant(ent.GetResource(), ent.GetSlug(), principal.Id)}
	annos := annotations.New()
	state, stateAnnos, err := client.OwnerState(ctx, enterprise, login)
	annos = freshestRateLimit(annos, stateAnnos)
	if err != nil {
		return nil, annos, err
	}
	if state.isOwner || state.pendingInvitationID != "" {
		annos.Append(&v2.GrantAlreadyExists{})
		return result, annos, nil
	}

	if inviteErr := client.InviteOwner(ctx, state.enterpriseID, login); inviteErr != nil {
		if status.Code(inviteErr) != codes.FailedPrecondition {
			return nil, annos, inviteErr
		}
		// Only the promotion's error is returned, so GitHub's reason for
		// refusing the invitation would otherwise be lost.
		ctxzap.Extract(ctx).Debug("baton-github: invitation rejected, promoting in place instead",
			zap.String("login", login),
			zap.Error(inviteErr),
		)
		if promoteErr := client.UpdateRole(
			ctx, state.enterpriseID, login, githubv4.EnterpriseAdministratorRoleOwner,
		); promoteErr != nil {
			// The promotion's status code is what tells C1 whether to retry.
			return nil, annos, fmt.Errorf(
				"promoting %s after the invitation was rejected: %w", login, promoteErr)
		}
	}

	state, stateAnnos, err = client.OwnerState(ctx, enterprise, login)
	annos = freshestRateLimit(annos, stateAnnos)
	if err != nil {
		return nil, annos, err
	}
	switch {
	case state.isOwner, state.pendingInvitationID != "":
		return result, annos, nil
	default:
		return nil, annos, status.Errorf(codes.Unavailable,
			"baton-github: enterprise owner grant for %s is not visible in GitHub", login)
	}
}

// Revoke takes the built-in Owner role away from a user.
//
// Holding the role and carrying an invitation are not alternatives, so both
// are cleared: either one left behind would fail the verification that
// follows. Demotion uses UNAFFILIATED, which keeps the user as a member of the
// enterprise instead of evicting them. A NOT_FOUND from either mutation is
// success, because it means the state being asked for is already in place.
func (o *enterpriseRoleResourceType) Revoke(
	ctx context.Context,
	grantObj *v2.Grant,
) (annotations.Annotations, error) {
	enterprise, client, err := o.provisioningTarget(ctx, grantObj.GetPrincipal(), grantObj.GetEntitlement())
	if err != nil {
		return nil, err
	}
	login, err := o.userLogin(ctx, grantObj.GetPrincipal().GetId().GetResource())
	if err != nil {
		return nil, err
	}

	annos := annotations.New()
	state, stateAnnos, err := client.OwnerState(ctx, enterprise, login)
	annos = freshestRateLimit(annos, stateAnnos)
	if err != nil {
		return annos, err
	}
	if !state.isOwner && state.pendingInvitationID == "" {
		annos.Append(&v2.GrantAlreadyRevoked{})
		return annos, nil
	}

	if state.isOwner {
		if err := client.UpdateRole(
			ctx, state.enterpriseID, login, enterpriseAdministratorRoleUnaffiliated,
		); err != nil && status.Code(err) != codes.NotFound {
			return annos, err
		}
	}
	if state.pendingInvitationID != "" {
		if err := client.CancelInvitation(ctx, state.pendingInvitationID); err != nil && status.Code(err) != codes.NotFound {
			return annos, err
		}
	}

	state, stateAnnos, err = client.OwnerState(ctx, enterprise, login)
	annos = freshestRateLimit(annos, stateAnnos)
	if err != nil {
		return annos, err
	}
	if state.isOwner || state.pendingInvitationID != "" {
		return annos, status.Errorf(codes.Unavailable,
			"baton-github: enterprise owner revoke for %s is not visible in GitHub", login)
	}

	return annos, nil
}

func (o *enterpriseRoleResourceType) provisioningTarget(
	ctx context.Context,
	principal *v2.Resource,
	ent *v2.Entitlement,
) (string, *githubEnterpriseAdministratorClient, error) {
	if principal.GetId().GetResourceType() != resourceTypeUser.Id {
		return "", nil, status.Error(codes.InvalidArgument,
			"baton-github: enterprise role can only be granted to a user")
	}
	enterprise, ok := provisionableEnterpriseOwner(ent.GetResource().GetId().GetResource())
	if !ok {
		return "", nil, status.Error(codes.InvalidArgument,
			"baton-github: only the built-in enterprise Owner role can be provisioned")
	}
	// The construction error comes first: without it the operator is told the
	// app is not installed even when the real cause was a transient failure
	// or a mismatched organization, and FailedPrecondition reads to C1 as
	// non-retryable.
	enterpriseClients, err := o.clients(ctx)
	if err != nil {
		return "", nil, err
	}
	client, ok := enterpriseClients[enterprise]
	if !ok {
		return "", nil, status.Errorf(codes.FailedPrecondition,
			"baton-github: cannot provision enterprise %s: %s", enterprise, o.noClientReason())
	}
	return enterprise, client, nil
}

func (o *enterpriseRoleResourceType) userLogin(ctx context.Context, userID string) (string, error) {
	id, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "baton-github: invalid GitHub user ID %q", userID)
	}
	user, resp, err := o.client.Users.GetByID(ctx, id)
	if err != nil {
		return "", wrapGitHubError(err, resp, fmt.Sprintf("baton-github: failed to get user %d", id))
	}
	if user.GetLogin() == "" {
		return "", fmt.Errorf("baton-github: GitHub user %d has no login", id)
	}
	return user.GetLogin(), nil
}

func enterpriseOwnerPrincipalID(owner enterpriseUser) (*v2.ResourceId, error) {
	if owner.databaseID <= 0 {
		return nil, fmt.Errorf("baton-github: enterprise owner %q has no database ID", owner.login)
	}
	principalId, err := resourceSdk.NewResourceID(resourceTypeUser, owner.databaseID)
	if err != nil {
		return nil, fmt.Errorf("baton-github: error creating resource ID for user %s: %w", owner.login, err)
	}
	return principalId, nil
}

// provisionableEnterpriseOwner returns the enterprise of a resource ID when it
// names the one role this connector can read and write through the GitHub App.
// Sync and provisioning share it so they cannot disagree on which roles are in
// the catalog.
func provisionableEnterpriseOwner(resourceID string) (string, bool) {
	enterprise, role, ok := parseEnterpriseRoleID(resourceID)
	if !ok || !strings.EqualFold(role, enterpriseRoleOwner) {
		return "", false
	}

	return enterprise, true
}

func parseEnterpriseRoleID(resourceID string) (string, string, bool) {
	enterprise, role, ok := strings.Cut(resourceID, ":")
	return enterprise, role, ok && enterprise != "" && role != ""
}
