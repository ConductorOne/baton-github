package connector

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rType "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
)

const (
	teamRoleMember     = "member"
	teamRoleMaintainer = "maintainer"

	// Pagination bag state for the team's pending invitations. Namespaced so it
	// cannot collide with the team role states, which double as bag states.
	teamStateInvitations = "team:invitations"
)

var teamAccessLevels = []string{
	teamRoleMember,
	teamRoleMaintainer,
}

// teamResource creates a new connector resource for a GitHub Team. It is possible that the team has a parent resource.
// orgID must be passed explicitly since ListTeams() doesn't return the full organization object.
func teamResource(team *github.Team, orgID int64, parentResourceID *v2.ResourceId) (*v2.Resource, error) {
	profile := map[string]interface{}{
		// Note: members_count and repos_count are only populated when fetching
		// individual teams. We skip the per-team API call to avoid N+1 requests.
		"members_count": team.GetMembersCount(),
		"repos_count":   team.GetReposCount(),
		// Store the org ID in the profile so that we can reference it when calculating grants
		"orgID": orgID,
	}

	ret, err := rType.NewGroupResource(
		team.GetName(),
		resourceTypeTeam,
		team.GetID(),
		[]rType.GroupTraitOption{},
		// profile has moved from GroupTrait to a Resource-level attribute.
		rType.WithResourceProfile(profile),
		rType.WithAnnotation(
			&v2.ExternalLink{Url: team.GetURL()},
			&v2.V1Identifier{Id: fmt.Sprintf("team:%d", team.GetID())},
		),
		rType.WithParentResourceID(parentResourceID),
	)
	if err != nil {
		return nil, err
	}

	return ret, nil
}

type teamResourceType struct {
	resourceType            *v2.ResourceType
	client                  *github.Client
	orgCache                *orgNameCache
	directCollaboratorsOnly bool
	reinviteForGrants       bool
}

func (o *teamResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *teamResourceType) List(ctx context.Context, parentID *v2.ResourceId, opts rType.SyncOpAttrs) ([]*v2.Resource, *rType.SyncOpResults, error) {
	if parentID == nil {
		return nil, &rType.SyncOpResults{}, nil
	}

	bag, page, err := parsePageToken(opts.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeTeam.Id})
	if err != nil {
		return nil, nil, err
	}

	listOpts := &github.ListOptions{
		Page:    page,
		PerPage: maxPageSize,
	}

	orgID, err := parseResourceToGitHub(parentID)
	if err != nil {
		return nil, nil, err
	}

	var rv []*v2.Resource

	orgName, err := o.orgCache.GetOrgName(ctx, opts.Session, parentID)
	if err != nil {
		return nil, nil, err
	}

	teams, resp, err := o.client.Teams.ListTeams(ctx, orgName, listOpts)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list teams")
	}

	pageToken, reqAnnos, err := nextPageToken(bag, resp)
	if err != nil {
		return nil, nil, err
	}

	for _, team := range teams {
		teamData := team
		if !o.directCollaboratorsOnly {
			fullTeam, resp, err := o.client.Teams.GetTeamByID(ctx, orgID, team.GetID()) //nolint:staticcheck,nolintlint // TODO: migrate to GetTeamBySlug
			if err != nil {
				return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to get team details")
			}
			teamData = fullTeam
		}

		tr, err := teamResource(teamData, orgID, &v2.ResourceId{ResourceType: resourceTypeOrg.Id, Resource: fmt.Sprintf("%d", orgID)})
		if err != nil {
			return nil, nil, err
		}

		rv = append(rv, tr)
	}

	return rv, &rType.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
}

func (o *teamResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ rType.SyncOpAttrs) ([]*v2.Entitlement, *rType.SyncOpResults, error) {
	return nil, nil, nil
}

func (o *teamResourceType) StaticEntitlements(_ context.Context, _ rType.SyncOpAttrs) ([]*v2.Entitlement, *rType.SyncOpResults, error) {
	rv := make([]*v2.Entitlement, 0, len(teamAccessLevels))
	for _, level := range teamAccessLevels {
		rv = append(
			rv,
			entitlement.NewPermissionEntitlement(
				nil,
				level,
				entitlement.WithDisplayName(fmt.Sprintf("Team %s", titleCase(level))),
				entitlement.WithDescription(fmt.Sprintf("Access to team in GitHub as %s", level)),
				entitlement.WithGrantableTo(resourceTypeUser, resourceTypeInvitation),
			),
		)
	}

	return rv, &rType.SyncOpResults{}, nil
}

func (o *teamResourceType) Grants(ctx context.Context, resource *v2.Resource, opts rType.SyncOpAttrs) ([]*v2.Grant, *rType.SyncOpResults, error) {
	bag, page, err := parsePageToken(opts.PageToken.Token, resource.Id)
	if err != nil {
		return nil, nil, err
	}

	// profile has moved from GroupTrait to a Resource-level attribute; read it
	// via GetProfile, which resolves the resource-level value (with a
	// trait-level fallback for older data).
	orgID, ok := rType.GetProfileInt64Value(rType.GetProfile(resource), "orgID")
	if !ok {
		return nil, nil, fmt.Errorf("error fetching orgID from team profile")
	}

	org, resp, err := o.client.Organizations.GetByID(ctx, orgID)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to get organization")
	}

	githubID, err := parseResourceToGitHub(resource.Id)
	if err != nil {
		return nil, nil, err
	}

	var (
		reqAnnos  annotations.Annotations
		pageToken string
		rv        = []*v2.Grant{}
	)
	switch rId := bag.ResourceTypeID(); rId {
	case resourceTypeTeam.Id:
		bag.Pop()
		// Pushed first so it drains last, after both accepted-member states.
		bag.Push(pagination.PageState{
			ResourceTypeID: teamStateInvitations,
		})
		bag.Push(pagination.PageState{
			ResourceTypeID: teamRoleMember,
		})
		bag.Push(pagination.PageState{
			ResourceTypeID: teamRoleMaintainer,
		})
	case teamStateInvitations:
		invitationGrants, nextPage, annos, err := o.pendingInvitationGrants(ctx, resource, org.GetLogin(), org.GetID(), githubID, page)
		if err != nil {
			return nil, nil, err
		}
		reqAnnos = annos
		rv = append(rv, invitationGrants...)

		if err := bag.Next(nextPage); err != nil {
			return nil, nil, err
		}
	case teamRoleMember, teamRoleMaintainer:
		listOpts := github.TeamListTeamMembersOptions{
			ListOptions: github.ListOptions{
				Page:    page,
				PerPage: maxPageSize,
			},
			Role: rId,
		}

		users, resp, err := o.client.Teams.ListTeamMembersByID(ctx, org.GetID(), githubID, &listOpts)
		if err != nil {
			if isNotFoundError(resp) {
				return nil, nil, uhttp.WrapErrors(codes.NotFound, fmt.Sprintf("org: %d not found", org.GetID()))
			}
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list team members")
		}

		var nextPage string
		nextPage, reqAnnos, err = parseResp(resp)
		if err != nil {
			return nil, nil, fmt.Errorf("github-connectorv2: failed to parse response: %w", err)
		}

		err = bag.Next(nextPage)
		if err != nil {
			return nil, nil, err
		}

		for _, user := range users {
			ur, err := userResource(ctx, user, user.GetEmail(), nil)
			if err != nil {
				return nil, nil, err
			}
			rv = append(rv, grant.NewGrant(resource, rId, ur.Id,
				grant.WithAnnotation(&v2.V1Identifier{
					Id: fmt.Sprintf("team-grant:%s:%d:%s", resource.Id.Resource, user.GetID(), rId),
				}),
			))
		}
	default:
		ctxzap.Extract(ctx).Warn("Unknown GitHub Role Name",
			zap.String("role_name", rId),
		)
	}

	pageToken, err = bag.Marshal()
	if err != nil {
		return nil, nil, err
	}
	return rv, &rType.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
}

// pendingInvitationGrants emits team grants for people who have been invited to
// the team but have not accepted yet. GitHub's team-members endpoints only
// return accepted members, so without this pass a birthright grant that C1 has
// already provisioned looks unfulfilled until the invitee clicks through.
//
// The listing returns organization-invitation objects, which carry the *org*
// role (direct_member/admin), not the team role. The team role comes from a
// per-invitation membership lookup, which is only possible for invitees GitHub
// can name; email-only invitations fall back to plain membership.
func (o *teamResourceType) pendingInvitationGrants(
	ctx context.Context,
	resource *v2.Resource,
	orgName string,
	orgID int64,
	teamID int64,
	page int,
) ([]*v2.Grant, string, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	listOpts := &github.ListOptions{Page: page, PerPage: maxPageSize}
	invitations, resp, err := o.client.Teams.ListPendingTeamInvitationsByID(ctx, orgID, teamID, listOpts)
	if err != nil {
		if isNotFoundError(resp) || isPermissionError(resp) {
			l.Debug("github-connector: cannot list pending team invitations, skipping",
				zap.String("org", orgName),
				zap.Int64("team_id", teamID),
				zap.String("github_error", gitHubErrorMessage(err)),
			)
			return nil, "", nil, nil
		}
		return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to list pending team invitations")
	}

	nextPage, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, err
	}

	rv := make([]*v2.Grant, 0, len(invitations))
	for _, inv := range invitations {
		role := o.pendingTeamRole(ctx, orgID, teamID, inv)
		rv = append(rv, grant.NewGrant(resource, role, invitationResourceID(inv.GetID()),
			grant.WithAnnotation(&v2.V1Identifier{
				Id: fmt.Sprintf("team-invitation-grant:%s:%d:%s", resource.Id.Resource, inv.GetID(), role),
			}),
		))
	}

	return rv, nextPage, reqAnnos, nil
}

// pendingTeamRole resolves the team role of a pending invitee, defaulting to
// member when GitHub cannot tell us (no login to query by, or the lookup fails).
func (o *teamResourceType) pendingTeamRole(ctx context.Context, orgID, teamID int64, inv *github.Invitation) string {
	login := inv.GetLogin()
	if login == "" {
		return teamRoleMember
	}

	membership, _, err := o.client.Teams.GetTeamMembershipByID(ctx, orgID, teamID, login)
	if err != nil {
		ctxzap.Extract(ctx).Debug("github-connector: could not read pending team membership role, assuming member",
			zap.Int64("team_id", teamID),
			zap.String("login", login),
			zap.String("github_error", gitHubErrorMessage(err)),
		)
		return teamRoleMember
	}

	if membership.GetRole() == teamRoleMaintainer {
		return teamRoleMaintainer
	}
	return teamRoleMember
}

func (o *teamResourceType) Grant(ctx context.Context, principal *v2.Resource, entitlement *v2.Entitlement) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	if principal.Id.ResourceType != resourceTypeUser.Id && !isInvitationPrincipal(principal) {
		l.Warn(
			"github-connectorv2: only users and invitations can be granted team membership",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connectorv2: only users and invitations can be granted team membership")
	}

	teamId, err := strconv.ParseInt(entitlement.Resource.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	if entitlement.GetResource().GetParentResourceId() == nil {
		return nil, fmt.Errorf("github-connectorv2: parent resource is required to grant team membership")
	}

	// FIXME(jirwin): Now that we've flattened out the team hierarchy, we don't need to check the parent type.
	// Leaving this check here for backwards compatability with the old model.
	var orgId int64
	switch entitlement.Resource.ParentResourceId.ResourceType {
	case resourceTypeOrg.Id:
		var err error
		orgId, err = strconv.ParseInt(entitlement.Resource.ParentResourceId.Resource, 10, 64)
		if err != nil {
			return nil, err
		}
	case resourceTypeTeam.Id:
		// profile has moved from GroupTrait to a Resource-level attribute; read
		// it via GetProfile (resource-level value, with a trait-level fallback).
		orgID, ok := rType.GetProfileInt64Value(rType.GetProfile(entitlement.Resource), "orgID")
		if !ok {
			return nil, fmt.Errorf("error fetching orgID from team profile")
		}

		orgId = orgID
	}

	enIDParts := strings.Split(entitlement.Id, ":")
	if len(enIDParts) != 3 {
		return nil, fmt.Errorf("github-connectorv2: invalid entitlement ID: %s", entitlement.Id)
	}
	permission := enIDParts[2]

	if isInvitationPrincipal(principal) {
		return o.grantToInvitation(ctx, principal, orgId, teamId, permission)
	}

	userId, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	user, resp, err := o.client.Users.GetByID(ctx, userId)
	if err != nil {
		return nil, wrapGitHubError(err, resp, fmt.Sprintf("github-connector: failed to get user %d", userId))
	}

	_, resp, er := o.client.Teams.AddTeamMembershipByID(
		ctx,
		orgId,
		teamId,
		user.GetLogin(),
		&github.TeamAddTeamMembershipOptions{Role: permission},
	)

	if er != nil {
		return nil, wrapGitHubError(er, resp, "github-connector: failed to add user to team")
	}

	return nil, nil
}

// grantToInvitation pre-stages team access for someone whose org invitation is
// still pending, so the access is already in place when they accept.
//
// Two paths, because GitHub gives us two very different amounts of room:
//
//   - The invitee has a GitHub login. Adding them to the team creates a pending
//     team membership, non-destructively, exactly as it would for a member.
//   - The invitation was sent to a bare email address. GitHub accepts team_ids
//     only when an invitation is created, so the only way to attach a team is to
//     re-issue the invitation — gated behind reinviteForGrants because it
//     invalidates the outstanding invite link.
func (o *teamResourceType) grantToInvitation(
	ctx context.Context,
	principal *v2.Resource,
	orgID int64,
	teamID int64,
	permission string,
) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	orgName, err := o.orgCache.GetOrgNameFromRemoteServer(ctx, strconv.FormatInt(orgID, 10))
	if err != nil {
		return nil, err
	}

	inv, err := parseInvitationPrincipal(ctx, o.client, orgName, principal)
	if err != nil {
		return nil, err
	}

	// Tracks a refusal from the direct path so the eventual error names the real
	// cause instead of blaming a missing login.
	var refusedErr error

	if login := inv.GetLogin(); login != "" {
		_, resp, err := o.client.Teams.AddTeamMembershipByID(ctx, orgID, teamID, login, &github.TeamAddTeamMembershipOptions{
			Role: permission,
		})
		if err == nil {
			return nil, nil
		}
		if isIdPManagedTeamError(err, resp) {
			return nil, uhttp.WrapErrors(codes.FailedPrecondition, fmt.Sprintf(
				"github-connector: team %d membership is managed by an external identity provider; grant the access "+
					"in the identity provider instead", teamID), err)
		}
		if !isNotAnOrgMemberError(err, resp) {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to add invited user to team")
		}
		// GitHub declined to invite through the team endpoint. Re-issuing the
		// invitation with the team attached is the remaining option.
		refusedErr = err
		l.Debug("github-connector: team membership rejected for pending invitee, falling back to re-invite",
			zap.Int64("invitation_id", inv.GetID()),
			zap.String("github_error", gitHubErrorMessage(err)),
		)
	}

	existingTeamIDs, err := invitationTeamIDs(ctx, o.client, orgName, inv.GetID())
	if err != nil {
		return nil, err
	}
	if slices.Contains(existingTeamIDs, teamID) {
		return annotations.New(&v2.GrantAlreadyExists{}), nil
	}

	if !o.reinviteForGrants {
		operation := fmt.Sprintf("add invitation to team %d", teamID)
		if refusedErr != nil {
			return nil, invitationRefusedError(operation, inv, refusedErr)
		}
		return nil, invitationNotProvisionableError(operation, inv)
	}

	if _, err := reinviteWithTeams(ctx, o.client, orgName, inv, append(existingTeamIDs, teamID)); err != nil {
		return nil, err
	}

	// The replacement invitation has a new ID, so the invitation resource this
	// grant was made against no longer exists. C1 reconciles on the next sync.
	l.Info("github-connector: pre-staged team access by re-issuing a pending invitation",
		zap.Int64("old_invitation_id", inv.GetID()),
		zap.Int64("team_id", teamID),
	)
	return nil, nil
}

func (o *teamResourceType) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	entitlement := grant.Entitlement
	principal := grant.Principal

	if principal.Id.ResourceType != resourceTypeUser.Id && !isInvitationPrincipal(principal) {
		l.Warn(
			"github-connectorv2: only users and invitations can have team membership revoked",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connectorv2: only users and invitations can have team membership revoked")
	}

	teamId, err := strconv.ParseInt(entitlement.Resource.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	if entitlement.GetResource().GetParentResourceId() == nil {
		return nil, fmt.Errorf("github-connectorv2: parent resource is required to revoke team membership")
	}

	orgId, err := strconv.ParseInt(entitlement.Resource.ParentResourceId.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	if isInvitationPrincipal(principal) {
		return o.revokeFromInvitation(ctx, principal, orgId, teamId)
	}

	userId, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	user, resp, err := o.client.Users.GetByID(ctx, userId)
	if err != nil {
		return nil, wrapGitHubError(err, resp, fmt.Sprintf("github-connector: failed to get user %d", userId))
	}
	resp, er := o.client.Teams.RemoveTeamMembershipByID(ctx, orgId, teamId, user.GetLogin())
	if er != nil {
		return nil, wrapGitHubError(er, resp, "github-connector: failed to revoke user team membership")
	}

	return nil, nil
}

// revokeFromInvitation withdraws a team from a still-pending invitation. As with
// granting, a named invitee can be removed directly; an email-only invitation can
// only have its team set changed by re-issuing it.
func (o *teamResourceType) revokeFromInvitation(
	ctx context.Context,
	principal *v2.Resource,
	orgID int64,
	teamID int64,
) (annotations.Annotations, error) {
	orgName, err := o.orgCache.GetOrgNameFromRemoteServer(ctx, strconv.FormatInt(orgID, 10))
	if err != nil {
		return nil, err
	}

	invitationID, err := strconv.ParseInt(principal.GetId().GetResource(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("github-connector: invalid invitation id %q: %w", principal.GetId().GetResource(), err)
	}

	inv, err := resolvePendingInvitation(ctx, o.client, orgName, invitationID)
	if err != nil {
		return nil, err
	}
	if inv == nil {
		// The invitation is gone, so the team membership it carried is too.
		return annotations.New(&v2.GrantAlreadyRevoked{}), nil
	}

	if login := inv.GetLogin(); login != "" {
		resp, err := o.client.Teams.RemoveTeamMembershipByID(ctx, orgID, teamID, login)
		if err != nil && !isNotFoundError(resp) {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to revoke invited user's team membership")
		}
		return nil, nil
	}

	existingTeamIDs, err := invitationTeamIDs(ctx, o.client, orgName, inv.GetID())
	if err != nil {
		return nil, err
	}
	if !slices.Contains(existingTeamIDs, teamID) {
		return annotations.New(&v2.GrantAlreadyRevoked{}), nil
	}
	remaining := slices.DeleteFunc(existingTeamIDs, func(id int64) bool { return id == teamID })

	if !o.reinviteForGrants {
		return nil, invitationNotProvisionableError(fmt.Sprintf("remove invitation from team %d", teamID), inv)
	}

	if _, err := reinviteWithTeams(ctx, o.client, orgName, inv, remaining); err != nil {
		return nil, err
	}
	return nil, nil
}

func TeamBuilder(client *github.Client, orgCache *orgNameCache, directCollaboratorsOnly bool, reinviteForGrants bool) *teamResourceType {
	return &teamResourceType{
		resourceType:            resourceTypeTeam,
		client:                  client,
		orgCache:                orgCache,
		directCollaboratorsOnly: directCollaboratorsOnly,
		reinviteForGrants:       reinviteForGrants,
	}
}
