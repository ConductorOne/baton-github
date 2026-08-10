package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/session"
	"github.com/conductorone/baton-sdk/pkg/types/sessions"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
)

// GitHub does not expose an endpoint to fetch a single organization invitation
// by ID, so resolving one means paging the pending-invitations list. This cap
// bounds that walk; orgs onboarding more than this many people at once will log
// a truncation warning rather than page forever.
const maxPendingInvitationPages = 25

// GitHub's org role vocabulary for invitations. Only "admin" maps to the org
// admin entitlement; every other role (direct_member, billing_manager,
// hiring_manager, reinstate) is plain membership as far as C1 is concerned.
const (
	invitationRoleDirectMember = "direct_member"
	invitationRoleAdmin        = "admin"
)

// isInvitationPrincipal reports whether a provisioning principal is a pending
// org invitation rather than an accepted GitHub user.
func isInvitationPrincipal(principal *v2.Resource) bool {
	return principal.GetId().GetResourceType() == resourceTypeInvitation.Id
}

// invitationNotProvisionableError explains why an invitation principal cannot
// receive the requested access. GitHub keys every post-hoc membership write off a
// username, so an invitation created from an email address alone has nothing to
// write against until the invitee has a GitHub account.
func invitationNotProvisionableError(operation string, inv *github.Invitation) error {
	return uhttp.WrapErrors(
		codes.FailedPrecondition,
		fmt.Sprintf(
			"github-connector: cannot %s for invitation %d: GitHub only accepts a username here, and this "+
				"invitation was sent to %q without a resolvable GitHub login. Either invite the user by GitHub "+
				"username (set github_username when creating the account) or turn on "+
				"reinvite-pending-invitations so the connector can re-issue the invitation with the requested "+
				"teams attached",
			operation, inv.GetID(), inv.GetEmail(),
		),
	)
}

// invitationRefusedError reports that GitHub declined a membership write for an
// invitee it *can* name. GitHub documents the team-membership endpoint as
// inviting non-members, but some org configurations refuse it while an
// invitation is already outstanding.
func invitationRefusedError(operation string, inv *github.Invitation, err error) error {
	return uhttp.WrapErrors(
		codes.FailedPrecondition,
		fmt.Sprintf(
			"github-connector: GitHub refused to %s for pending invitation %d (%s): %s. Turn on "+
				"reinvite-pending-invitations to let the connector re-issue the invitation with the requested "+
				"teams attached, or wait until the user accepts their org invitation",
			operation, inv.GetID(), inv.GetLogin(), gitHubErrorMessage(err),
		),
		err,
	)
}

// resolvePendingInvitation finds a pending org invitation by its numeric ID.
// Returns (nil, nil) when the invitation is no longer pending — it was accepted,
// cancelled, or expired since the last sync.
func resolvePendingInvitation(
	ctx context.Context,
	client *github.Client,
	orgName string,
	invitationID int64,
) (*github.Invitation, error) {
	l := ctxzap.Extract(ctx)
	opts := &github.ListOptions{PerPage: maxPageSize}
	for page := 0; page < maxPendingInvitationPages; page++ {
		invitations, resp, err := client.Organizations.ListPendingOrgInvitations(ctx, orgName, opts)
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to list pending org invitations")
		}
		for _, inv := range invitations {
			if inv.GetID() == invitationID {
				return inv, nil
			}
		}
		if resp.NextPage == 0 {
			return nil, nil
		}
		opts.Page = resp.NextPage
	}
	l.Warn("github-connector: gave up looking for pending invitation",
		zap.Int64("invitation_id", invitationID),
		zap.String("org", orgName),
		zap.Int("pages_searched", maxPendingInvitationPages),
	)
	return nil, nil
}

// parseInvitationPrincipal resolves an invitation principal to the live GitHub
// invitation it names. It fails rather than returning nil so callers do not have
// to distinguish "not an invitation" from "invitation vanished".
func parseInvitationPrincipal(
	ctx context.Context,
	client *github.Client,
	orgName string,
	principal *v2.Resource,
) (*github.Invitation, error) {
	invitationID, err := strconv.ParseInt(principal.GetId().GetResource(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("github-connector: invalid invitation id %q: %w", principal.GetId().GetResource(), err)
	}

	inv, err := resolvePendingInvitation(ctx, client, orgName, invitationID)
	if err != nil {
		return nil, err
	}
	if inv == nil {
		return nil, uhttp.WrapErrors(
			codes.FailedPrecondition,
			fmt.Sprintf(
				"github-connector: invitation %d is no longer pending in org %s; it was accepted, cancelled, or "+
					"expired. Retry against the accepted user once the next sync has run",
				invitationID, orgName,
			),
		)
	}
	return inv, nil
}

// invitationTeamIDs returns the IDs of the teams already attached to a pending
// invitation.
func invitationTeamIDs(
	ctx context.Context,
	client *github.Client,
	orgName string,
	invitationID int64,
) ([]int64, error) {
	var (
		teamIDs []int64
		opts    = &github.ListOptions{PerPage: maxPageSize}
	)
	for {
		teams, resp, err := client.Organizations.ListOrgInvitationTeams(ctx, orgName, strconv.FormatInt(invitationID, 10), opts)
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to list invitation teams")
		}
		for _, team := range teams {
			teamIDs = append(teamIDs, team.GetID())
		}
		if resp.NextPage == 0 {
			return teamIDs, nil
		}
		opts.Page = resp.NextPage
	}
}

// reinviteWithTeams replaces a pending invitation with an equivalent one carrying
// teamIDs.
//
// GitHub has no endpoint that modifies a pending invitation, and team_ids can
// only be supplied at creation time, so attaching a team to an existing
// email-only invitation requires cancel-then-recreate. That is destructive: the
// original invitation link stops working, the invitee receives a second email,
// the 7-day expiry clock restarts, and the invitation ID changes (so C1 sees the
// old invitation resource disappear and a new one appear on the next sync).
// Because create rejects a duplicate invitation, the cancel must land first; if
// the create then fails, this restores the original team set on a best-effort
// basis before returning.
func reinviteWithTeams(
	ctx context.Context,
	client *github.Client,
	orgName string,
	inv *github.Invitation,
	teamIDs []int64,
) (*github.Invitation, error) {
	l := ctxzap.Extract(ctx)

	opts, err := reinviteOptions(ctx, client, inv, teamIDs)
	if err != nil {
		return nil, err
	}

	originalTeamIDs, err := invitationTeamIDs(ctx, client, orgName, inv.GetID())
	if err != nil {
		return nil, err
	}

	resp, err := client.Organizations.CancelInvite(ctx, orgName, inv.GetID())
	if err != nil && !isNotFoundError(resp) {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to cancel invitation before re-inviting")
	}

	newInv, resp, err := client.Organizations.CreateOrgInvitation(ctx, orgName, opts)
	if err == nil {
		l.Info("github-connector: re-issued org invitation to change its teams",
			zap.Int64("old_invitation_id", inv.GetID()),
			zap.Int64("new_invitation_id", newInv.GetID()),
			zap.Int64s("team_ids", teamIDs),
		)
		return newInv, nil
	}

	createErr := wrapGitHubError(err, resp, "github-connector: failed to re-issue invitation with updated teams")

	// The cancel already landed, so the invitee currently has no invitation at
	// all. Put the original one back so a failed grant does not silently strip
	// access the user already had.
	restoreOpts, restoreOptsErr := reinviteOptions(ctx, client, inv, originalTeamIDs)
	if restoreOptsErr != nil {
		l.Error("github-connector: could not rebuild original invitation after a failed re-invite",
			zap.Int64("invitation_id", inv.GetID()), zap.Error(restoreOptsErr))
		return nil, createErr
	}
	if _, _, restoreErr := client.Organizations.CreateOrgInvitation(ctx, orgName, restoreOpts); restoreErr != nil {
		l.Error("github-connector: re-invite failed and the original invitation could not be restored; the user has no pending invitation",
			zap.Int64("invitation_id", inv.GetID()),
			zap.String("email", inv.GetEmail()),
			zap.String("login", inv.GetLogin()),
			zap.Error(restoreErr),
		)
	}
	return nil, createErr
}

// reinviteOptions rebuilds the create-invitation payload for an existing
// invitation, preserving its invitee and org role. invitee_id is preferred over
// email so the replacement invitation keeps a resolvable GitHub login.
func reinviteOptions(
	ctx context.Context,
	client *github.Client,
	inv *github.Invitation,
	teamIDs []int64,
) (*github.CreateOrgInvitationOptions, error) {
	role := inv.GetRole()
	if role == "" {
		role = invitationRoleDirectMember
	}
	opts := &github.CreateOrgInvitationOptions{
		Role:   github.Ptr(role),
		TeamID: teamIDs,
	}

	if login := inv.GetLogin(); login != "" {
		user, resp, err := client.Users.Get(ctx, login)
		if err != nil {
			return nil, wrapGitHubError(err, resp, fmt.Sprintf("github-connector: failed to resolve invitation login %q", login))
		}
		opts.InviteeID = github.Ptr(user.GetID())
		return opts, nil
	}

	if email := inv.GetEmail(); email != "" {
		opts.Email = github.Ptr(email)
		return opts, nil
	}

	return nil, fmt.Errorf("github-connector: invitation %d has neither a login nor an email to re-invite", inv.GetID())
}

// isNotAnOrgMemberError matches GitHub's refusal to act on a user who has not yet
// accepted their org invitation.
//
// PUT /orgs/{org}/teams/{team}/memberships/{username} is documented to invite
// non-members and leave the membership pending, and the "User isn't a member of
// this organization. Please invite them first." 422 belongs to the deprecated
// PUT /teams/{id}/members/{username} endpoint, which this connector does not
// use. This guard therefore covers the undocumented case: a write refused
// because an org invitation for that user is already outstanding.
func isNotAnOrgMemberError(err error, resp *github.Response) bool {
	return isGitHubValidationError(err, resp, "not a member", "isn't a member", "invite them first", "already invited")
}

// isIdPManagedTeamError matches the rejection GitHub returns for teams whose
// membership is owned by an external identity provider.
func isIdPManagedTeamError(err error, resp *github.Response) bool {
	return isGitHubValidationError(err, resp, "team synchronization", "externally managed", "identity provider")
}

// pendingInvitationLoginsKey is the per-org session key holding the
// lowercased-login-to-invitation-ID index built by pendingInvitationsByLogin.
func pendingInvitationLoginsKey(orgResourceID string) string {
	return "pending_invitation_logins:" + orgResourceID
}

// pendingInvitationsByLogin returns a lowercased-GitHub-login to invitation-ID
// index of the org's pending invitations, cached for the duration of the sync.
//
// Repository invitations identify their invitee by GitHub user, not by org
// invitation, so correlating one back to the invitation resource C1 synced needs
// this index.
func pendingInvitationsByLogin(
	ctx context.Context,
	client *github.Client,
	ss sessions.SessionStore,
	orgName string,
	orgResourceID string,
) (map[string]int64, error) {
	key := pendingInvitationLoginsKey(orgResourceID)
	cached, found, err := session.GetJSON[map[string]int64](ctx, ss, key)
	if err != nil {
		return nil, fmt.Errorf("baton-github: error reading pending invitation index from session: %w", err)
	}
	if found {
		return cached, nil
	}

	index := make(map[string]int64)
	opts := &github.ListOptions{PerPage: maxPageSize}
	for {
		invitations, resp, err := client.Organizations.ListPendingOrgInvitations(ctx, orgName, opts)
		if err != nil {
			if isNotFoundError(resp) || isPermissionError(resp) {
				// Without invitation visibility we simply cannot correlate repo
				// invitations; cache the empty index so every repo in this sync
				// does not retry the same failing call.
				ctxzap.Extract(ctx).Debug("github-connector: cannot list pending org invitations, skipping invitation-backed repo grants",
					zap.String("org", orgName),
					zap.String("github_error", gitHubErrorMessage(err)),
				)
				break
			}
			return nil, wrapGitHubError(err, resp, "github-connector: failed to list pending org invitations")
		}
		for _, inv := range invitations {
			if login := inv.GetLogin(); login != "" {
				index[strings.ToLower(login)] = inv.GetID()
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	if err := session.SetJSON(ctx, ss, key, index); err != nil {
		return nil, fmt.Errorf("baton-github: error caching pending invitation index: %w", err)
	}
	return index, nil
}

// invitationResourceID builds the resource ID for a pending invitation so grants
// point at the same resource invitationResourceType.List emits.
func invitationResourceID(invitationID int64) *v2.ResourceId {
	return &v2.ResourceId{
		ResourceType: resourceTypeInvitation.Id,
		Resource:     strconv.FormatInt(invitationID, 10),
	}
}
