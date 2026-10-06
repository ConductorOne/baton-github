package connector

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/conductorone/baton-github/pkg/customclient"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
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
type enterpriseClientProvider func(ctx context.Context) (map[string]*customclient.EnterpriseAdminClient, error)

type enterpriseRoleResourceType struct {
	resourceType   *v2.ResourceType
	client         *github.Client
	customClient   *customclient.Client
	enterprises    []string
	roleUsersCache map[string][]string
	mu             *sync.Mutex
	// newEnterpriseClients is nil under PAT auth.
	newEnterpriseClients enterpriseClientProvider
	// enterpriseClients is keyed by enterprise slug and memoized on success only.
	enterpriseClients map[string]*customclient.EnterpriseAdminClient
}

func (o *enterpriseRoleResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

// clients builds the per-enterprise administration clients on first use and
// memoizes them. Failures are not memoized, so a later sync retries after the
// operator fixes the installation.
func (o *enterpriseRoleResourceType) clients(
	ctx context.Context,
) (map[string]*customclient.EnterpriseAdminClient, error) {
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

// noClientReason explains why no administration client exists.
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
	for _, enterprise := range o.enterprises {
		// GitHub's consumed-licenses API is 1-indexed; page 0 is undocumented
		// and may return the same results as page 1, causing duplicates.
		page := 1
		continuePagination := true
		for continuePagination {
			consumedLicenses, _, err := o.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise, page)
			if err != nil {
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
		if isEnterpriseSetupError(err) {
			warnEnterpriseRolesSkipped(ctx, err)
			return nil, &resourceSdk.SyncOpResults{}, nil
		}
		return nil, nil, failClosedOnUnreadableEnterprise(err, strings.Join(o.enterprises, ", "))
	}

	// Under app auth only the Owner role is readable; consumed-licenses is PAT-only.
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
		if isEnterpriseSetupError(err) {
			warnEnterpriseRolesSkipped(ctx, err)
			return nil, &resourceSdk.SyncOpResults{}, nil
		}
		return nil, nil, failClosedOnUnreadableEnterprise(err, strings.Join(o.enterprises, ", "))
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

var _ connectorbuilder.ResourceProvisionerV2 = (*enterpriseRoleProvisioner)(nil)

// enterpriseRoleProvisioner adds Grant and Revoke to the read-only syncer. Under
// PAT auth both fail, naming the credential they need.
type enterpriseRoleProvisioner struct {
	*enterpriseRoleResourceType
}

// EnterpriseRoleProvisioningBuilder returns the syncer with Grant and Revoke.
func EnterpriseRoleProvisioningBuilder(
	client *github.Client,
	customClient *customclient.Client,
	enterprises []string,
	newEnterpriseClients enterpriseClientProvider,
) *enterpriseRoleProvisioner {
	return &enterpriseRoleProvisioner{
		enterpriseRoleResourceType: EnterpriseRoleBuilder(
			client, customClient, enterprises, newEnterpriseClients),
	}
}

// EnterpriseRoleBuilder returns the read-only enterprise role syncer.
func EnterpriseRoleBuilder(
	client *github.Client,
	customClient *customclient.Client,
	enterprises []string,
	newEnterpriseClients enterpriseClientProvider,
) *enterpriseRoleResourceType {
	return &enterpriseRoleResourceType{
		resourceType:         resourceTypeEnterpriseRole,
		client:               client,
		customClient:         customClient,
		enterprises:          enterprises,
		roleUsersCache:       make(map[string][]string),
		mu:                   &sync.Mutex{},
		newEnterpriseClients: newEnterpriseClients,
	}
}

// appList emits only the built-in Owner role.
func appList(
	enterprises []string,
	enterpriseClients map[string]*customclient.EnterpriseAdminClient,
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

// appGrants emits current owners and pending invitees on the same entitlement,
// since C1 has no pending grant state. Owners and invitations are two phases of
// one page token, because invitations are resolved from the enterprise members.
func (o *enterpriseRoleResourceType) appGrants(
	ctx context.Context,
	enterpriseClients map[string]*customclient.EnterpriseAdminClient,
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
		return nil, &resourceSdk.SyncOpResults{Annotations: annos}, failClosedOnUnreadableEnterprise(err, enterprise)
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

// enterpriseSetupError marks a client build that failed on how the app or
// --enterprises is configured, as opposed to GitHub being unreachable. Before
// the enterprise administrator API was used, such a deployment synced no
// enterprise roles without failing, so sync keeps doing that; Grant and Revoke
// still return the error, which names the fix.
type enterpriseSetupError struct{ error }

func (e enterpriseSetupError) Unwrap() error { return e.error }

func isEnterpriseSetupError(err error) bool {
	var setupErr enterpriseSetupError
	return errors.As(err, &setupErr)
}

// asEnterpriseSetupError marks the organization mismatch as a configuration
// problem. It keys on the sentinel rather than on FailedPrecondition, because
// the client wraps its query failures with %w and status.Code unwraps through
// them: a GraphQL UNPROCESSABLE during the walk carries that same code, and
// skipping the sync on one would delete every Owner grant for a reason that
// has nothing to do with how the deployment is configured.
func asEnterpriseSetupError(err error) error {
	if !errors.Is(err, customclient.ErrOrganizationNotInEnterprise) {
		return err
	}

	return enterpriseSetupError{err}
}

func warnEnterpriseRolesSkipped(ctx context.Context, err error) {
	ctxzap.Extract(ctx).Warn("baton-github: skipping enterprise roles, the GitHub App is not set up to read them",
		zap.Error(err))
}

// failClosedOnUnreadableEnterprise turns NotFound into FailedPrecondition. The SDK
// downgrades NotFound to a warning and completes the sync, which would make C1
// delete every Owner grant.
func failClosedOnUnreadableEnterprise(err error, enterprise string) error {
	if status.Code(err) != codes.NotFound {
		return err
	}

	return status.Errorf(codes.FailedPrecondition,
		"baton-github: enterprise %q or its configured organization could not be read, so no owners can be "+
			"listed; failing rather than reporting none, which would revoke every Owner grant: %v",
		enterprise, err)
}

// ownerGrants emits one page of the users who hold the role today.
func (o *enterpriseRoleResourceType) ownerGrants(
	ctx context.Context,
	client *customclient.EnterpriseAdminClient,
	resource *v2.Resource,
	after *githubv4.String,
) ([]*v2.Grant, string, annotations.Annotations, error) {
	owners, nextCursor, annos, err := client.Owners(ctx, after)
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

// pendingInvitationGrants emits one page of pending Owner invitees, resolved by
// looking up the enterprise members, since invitations can't be listed with an
// installation token. Invitations to non-members are not visible.
func (o *enterpriseRoleResourceType) pendingInvitationGrants(
	ctx context.Context,
	client *customclient.EnterpriseAdminClient,
	resource *v2.Resource,
	enterprise string,
	after *githubv4.String,
) ([]*v2.Grant, string, annotations.Annotations, error) {
	members, nextCursor, annos, err := client.Members(ctx, enterprise, after)
	if err != nil {
		return nil, "", annos, err
	}
	if len(members) == 0 {
		return nil, nextCursor, annos, nil
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

	ret := make([]*v2.Grant, 0, len(invitations))
	for _, member := range members {
		if _, invited := invitations[member.Login]; !invited {
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

// Grant gives a user the built-in Owner role by inviting them. Existing
// administrators are refused rather than promoted in place: their current role
// can't be read, so Revoke could not restore it. A pending invitation counts as
// granted, matching Grants().
func (o *enterpriseRoleProvisioner) Grant(
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
	if state.HoldsRole() {
		annos.Append(&v2.GrantAlreadyExists{})
		return result, annos, nil
	}

	// Grants() can only see invitations to enterprise members.
	isMember, memberAnnos, err := client.IsMember(ctx, enterprise, login)
	annos = freshestRateLimit(annos, memberAnnos)
	if err != nil {
		return nil, annos, err
	}
	if !isMember {
		return nil, annos, status.Errorf(codes.InvalidArgument,
			"baton-github: %s is not a member of enterprise %s; inviting them as Owner would create an "+
				"invitation this connector cannot read back, and the grant would disappear on the next sync",
			login, enterprise)
	}

	if inviteErr := client.InviteOwner(ctx, state.EnterpriseID, login); inviteErr != nil {
		if status.Code(inviteErr) != codes.FailedPrecondition {
			return nil, annos, inviteErr
		}
		ctxzap.Extract(ctx).Debug("baton-github: invitation rejected; refusing unsafe in-place promotion",
			zap.String("login", login),
			zap.Error(inviteErr),
		)
		// The rejection can also be a seat limit or SSO restriction, so let GitHub's
		// error give the reason.
		return nil, annos, fmt.Errorf(
			"baton-github: cannot grant enterprise Owner to %s, and promoting in place is not attempted "+
				"because a prior administrator role cannot be read back and revoke would discard it: %w",
			login, inviteErr)
	}

	state, stateAnnos, err = client.OwnerState(ctx, enterprise, login)
	annos = freshestRateLimit(annos, stateAnnos)
	if err != nil {
		return nil, annos, err
	}
	if !state.HoldsRole() {
		return nil, annos, status.Errorf(codes.Unavailable,
			"baton-github: enterprise owner grant for %s is not visible in GitHub", login)
	}

	return result, annos, nil
}

// Revoke removes the Owner role and cancels any pending invitation. Demotion uses
// UNAFFILIATED, which keeps the user's enterprise membership. NOT_FOUND from
// either mutation means it's already done.
func (o *enterpriseRoleProvisioner) Revoke(
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
	if !state.HoldsRole() {
		annos.Append(&v2.GrantAlreadyRevoked{})
		return annos, nil
	}

	if state.IsOwner {
		if err := client.UpdateRole(
			ctx, state.EnterpriseID, login, enterpriseAdministratorRoleUnaffiliated,
		); err != nil && status.Code(err) != codes.NotFound {
			return annos, err
		}
	}
	if state.PendingInvitationID != "" {
		if err := client.CancelInvitation(ctx, state.PendingInvitationID); err != nil && status.Code(err) != codes.NotFound {
			return annos, err
		}
	}

	state, stateAnnos, err = client.OwnerState(ctx, enterprise, login)
	annos = freshestRateLimit(annos, stateAnnos)
	if err != nil {
		return annos, err
	}
	if state.HoldsRole() {
		return annos, status.Errorf(codes.Unavailable,
			"baton-github: enterprise owner revoke for %s is not visible in GitHub", login)
	}

	return annos, nil
}

func (o *enterpriseRoleProvisioner) provisioningTarget(
	ctx context.Context,
	principal *v2.Resource,
	ent *v2.Entitlement,
) (string, *customclient.EnterpriseAdminClient, error) {
	if principal.GetId().GetResourceType() != resourceTypeUser.Id {
		return "", nil, status.Error(codes.InvalidArgument,
			"baton-github: enterprise role can only be granted to a user")
	}
	enterprise, ok := provisionableEnterpriseOwner(ent.GetResource().GetId().GetResource())
	if !ok {
		return "", nil, status.Error(codes.InvalidArgument,
			"baton-github: only the built-in enterprise Owner role can be provisioned")
	}
	// The build error comes first: the generic reason below cannot name a cause.
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

func enterpriseOwnerPrincipalID(owner customclient.EnterpriseUser) (*v2.ResourceId, error) {
	if owner.DatabaseID <= 0 {
		return nil, fmt.Errorf("baton-github: enterprise owner %q has no database ID", owner.Login)
	}
	principalId, err := resourceSdk.NewResourceID(resourceTypeUser, owner.DatabaseID)
	if err != nil {
		return nil, fmt.Errorf("baton-github: error creating resource ID for user %s: %w", owner.Login, err)
	}
	return principalId, nil
}

// provisionableEnterpriseOwner returns the enterprise of a resource ID that names
// the Owner role. Shared by sync and provisioning.
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
