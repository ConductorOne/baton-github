package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

const (
	// Profile keys for invitation status metadata.
	invitationProfileKeyStatus    = "invitation_status"
	invitationProfileKeyExpiresAt = "invitation_expires_at"

	// Values exposed via invitation_status.
	invitationStatusPendingAcceptance = "invitation_pending_acceptance"
	invitationStatusExpired           = "invitation_expired"

	// Pagination bag states used to drive the two upstream listing endpoints.
	invitationStatePending = "invitation:pending"
	invitationStateFailed  = "invitation:failed"

	// GitHub's failed_reason is a free-form sentence, not an enum, so we
	// substring-match (case-insensitive) for this token to detect
	// expiration. Observed format on api.github.com today:
	//   "Invitation expired. User did not accept this invite for 7 days"
	githubInvitationFailedReasonExpired = "expired"

	// Organization invitations expire 7 days after creation.
	// https://github.blog/changelog/2020-02-05-self-expiring-repository-and-organization-invitations/
	invitationLifetime = 7 * 24 * time.Hour
)

func invitationToUserResource(invitation *github.Invitation, status string) (*v2.Resource, error) {
	login := invitation.GetLogin()
	if login == "" {
		login = invitation.GetEmail()
	}

	profile := map[string]interface{}{
		"login":                    login,
		"inviter":                  invitation.GetInviter().GetLogin(),
		invitationProfileKeyStatus: status,
	}
	if expiresAt, ok := invitationExpiresAt(invitation, status); ok {
		profile[invitationProfileKeyExpiresAt] = expiresAt.UTC().Format(time.RFC3339)
	}

	ret, err := resourceSdk.NewUserResource(
		login,
		resourceTypeInvitation,
		invitation.GetID(),
		[]resourceSdk.UserTraitOption{
			resourceSdk.WithEmail(invitation.GetEmail(), true),
			resourceSdk.WithUserProfile(profile),
			resourceSdk.WithStatus(v2.UserTrait_Status_STATUS_UNSPECIFIED),
			resourceSdk.WithUserLogin(login),
		},
	)
	if err != nil {
		return nil, err
	}
	return ret, nil
}

// invitationExpiresAt returns the moment the invitation expired (for already
// expired invitations) or will expire (for pending invitations). GitHub does
// not surface an expires_at field on the org invitation payload, so for
// pending invitations we derive it from created_at + 7 days.
func invitationExpiresAt(invitation *github.Invitation, status string) (time.Time, bool) {
	if status == invitationStatusExpired {
		if t := invitation.GetFailedAt(); !t.IsZero() {
			return t.Time, true
		}
	}
	if t := invitation.GetCreatedAt(); !t.IsZero() {
		return t.Add(invitationLifetime), true
	}
	return time.Time{}, false
}

type invitationResourceType struct {
	client   *github.Client
	orgCache *orgNameCache
	orgs     []string
}

func (i *invitationResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return resourceTypeInvitation
}

func (i *invitationResourceType) List(ctx context.Context, parentID *v2.ResourceId, opts resourceSdk.SyncOpAttrs) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	if parentID == nil {
		return nil, &resourceSdk.SyncOpResults{}, nil
	}

	bag, page, err := parsePageToken(opts.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeInvitation.Id})
	if err != nil {
		return nil, nil, err
	}

	orgName, err := i.orgCache.GetOrgName(ctx, opts.Session, parentID)
	if err != nil {
		return nil, nil, err
	}

	listOpts := &github.ListOptions{
		Page:    page,
		PerPage: opts.PageToken.Size,
	}

	var (
		invitationResources []*v2.Resource
		respAnnos           annotations.Annotations
	)

	switch bag.ResourceTypeID() {
	case resourceTypeInvitation.Id:
		// First call: fan out into the two listing states. Pending is pushed
		// last so it is processed first; failed/expired runs after pending
		// fully drains.
		bag.Pop()
		bag.Push(pagination.PageState{ResourceTypeID: invitationStateFailed})
		bag.Push(pagination.PageState{ResourceTypeID: invitationStatePending})

	case invitationStatePending:
		invitations, resp, err := i.client.Organizations.ListPendingOrgInvitations(ctx, orgName, listOpts)
		if err != nil {
			if isNotFoundError(resp) {
				if err := bag.Next(""); err != nil {
					return nil, nil, err
				}
				break
			}
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list pending org invitations")
		}

		nextPage, annos, err := parseResp(resp)
		if err != nil {
			return nil, nil, err
		}
		respAnnos = annos

		if err := bag.Next(nextPage); err != nil {
			return nil, nil, err
		}

		invitationResources = make([]*v2.Resource, 0, len(invitations))
		for _, invitation := range invitations {
			ir, err := invitationToUserResource(invitation, invitationStatusPendingAcceptance)
			if err != nil {
				return nil, nil, err
			}
			invitationResources = append(invitationResources, ir)
		}

	case invitationStateFailed:
		invitations, resp, err := i.client.Organizations.ListFailedOrgInvitations(ctx, orgName, listOpts)
		if err != nil {
			if isNotFoundError(resp) {
				if err := bag.Next(""); err != nil {
					return nil, nil, err
				}
				break
			}
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list failed org invitations")
		}

		nextPage, annos, err := parseResp(resp)
		if err != nil {
			return nil, nil, err
		}
		respAnnos = annos

		if err := bag.Next(nextPage); err != nil {
			return nil, nil, err
		}

		l := ctxzap.Extract(ctx)
		invitationResources = make([]*v2.Resource, 0, len(invitations))
		for _, invitation := range invitations {
			// The failed_invitations endpoint includes failures other than
			// expirations (e.g. user_was_inactive, unexpected_failure). Only
			// surface invitations that explicitly expired.
			failedReason := invitation.GetFailedReason()
			if !strings.Contains(strings.ToLower(failedReason), githubInvitationFailedReasonExpired) {
				l.Debug("skipping non-expired failed invitation",
					zap.Int64("invitation_id", invitation.GetID()),
					zap.String("failed_reason", failedReason),
				)
				continue
			}

			ir, err := invitationToUserResource(invitation, invitationStatusExpired)
			if err != nil {
				return nil, nil, err
			}
			invitationResources = append(invitationResources, ir)
		}

	default:
		return nil, nil, fmt.Errorf("github-connector: unexpected invitation page state %q", bag.ResourceTypeID())
	}

	pageToken, err := bag.Marshal()
	if err != nil {
		return nil, nil, err
	}

	return invitationResources, &resourceSdk.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   respAnnos,
	}, nil
}

func (i *invitationResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ resourceSdk.SyncOpAttrs) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	return nil, &resourceSdk.SyncOpResults{}, nil
}

func (i *invitationResourceType) Grants(ctx context.Context, resource *v2.Resource, opts resourceSdk.SyncOpAttrs) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	return nil, &resourceSdk.SyncOpResults{}, nil
}

func (i *invitationResourceType) CreateAccountCapabilityDetails(ctx context.Context) (*v2.CredentialDetailsAccountProvisioning, annotations.Annotations, error) {
	return &v2.CredentialDetailsAccountProvisioning{
		SupportedCredentialOptions: []v2.CapabilityDetailCredentialOption{
			v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_NO_PASSWORD,
		},
		PreferredCredentialOption: v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_NO_PASSWORD,
	}, nil, nil
}

func (i *invitationResourceType) CreateAccount(
	ctx context.Context,
	accountInfo *v2.AccountInfo,
	credentialOptions *v2.LocalCredentialOptions,
) (
	connectorbuilder.CreateAccountResponse,
	[]*v2.PlaintextData,
	annotations.Annotations,
	error,
) {
	l := ctxzap.Extract(ctx)

	params, err := getCreateUserParams(accountInfo)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("github-connectorv2: failed to get CreateUserParams: %w", err)
	}

	invitation, resp, err := i.client.Organizations.CreateOrgInvitation(ctx, params.org, &github.CreateOrgInvitationOptions{
		Email: params.email,
	})
	if err != nil {
		if isAlreadyOrgMemberError(err, resp) {
			memberResource, lookupErr := i.lookupUser(ctx, params.login, *params.email)
			if lookupErr != nil {
				l.Warn("failed to look up existing org member, returning AlreadyExistsResult without resource", zap.Error(lookupErr))
				return &v2.CreateAccountResponse_AlreadyExistsResult{}, nil, nil, nil
			}
			return &v2.CreateAccountResponse_AlreadyExistsResult{Resource: memberResource}, nil, nil, nil
		}
		if isAlreadyInvitedError(err, resp) {
			invitationResource, lookupErr := i.lookupPendingInvitation(ctx, params.org, params.login, *params.email)
			if lookupErr != nil {
				l.Warn("failed to look up existing invitation", zap.Error(lookupErr))
			} else if invitationResource == nil {
				l.Warn("pending invitation not found despite 'already invited' response from GitHub",
					zap.String("org", params.org), zap.String("email", *params.email))
			}
			return &v2.CreateAccountResponse_ActionRequiredResult{
				Resource: invitationResource,
				Message:  "GitHub org invite already pending. User must accept the existing invitation.",
			}, nil, nil, nil
		}
		if isEMUOrgError(err, resp) {
			return nil, nil, nil, fmt.Errorf("github-connector: organization %s uses Enterprise Managed Users (EMU); accounts are provisioned by the IdP, not via org invitations", params.org)
		}

		// Check for expired/failed invitations as diagnostic context for unexpected failures.
		failedInv, failedErr := i.lookupFailedInvitation(ctx, params.org, params.login, *params.email)
		if failedErr != nil {
			l.Warn("failed to check for expired invitations", zap.Error(failedErr))
		}
		if failedInv != nil {
			l.Warn("previous invitation expired or failed",
				zap.String("failed_reason", failedInv.GetFailedReason()),
				zap.Time("failed_at", failedInv.GetFailedAt().Time),
			)
		}

		return nil, nil, nil, wrapGitHubError(err, resp, "github-connector: failed to create org invitation")
	}

	restApiRateLimit, err := extractRateLimitData(resp)
	if err != nil {
		return nil, nil, nil, err
	}

	var annotations annotations.Annotations
	annotations.WithRateLimiting(restApiRateLimit)

	r, err := invitationToUserResource(invitation, invitationStatusPendingAcceptance)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("github-connectorv2: cannot create user resource: %w", err)
	}
	return &v2.CreateAccountResponse_ActionRequiredResult{
		Resource: r,
		Message:  "GitHub org invite sent. User must accept the invitation before team membership can be granted.",
	}, nil, annotations, nil
}

func (i *invitationResourceType) Delete(ctx context.Context, resourceId *v2.ResourceId) (annotations.Annotations, error) {
	if resourceId.ResourceType != resourceTypeInvitation.Id {
		return nil, fmt.Errorf("baton-github: non-invitation resource passed to invitation delete")
	}

	orgs, err := getOrgs(ctx, i.client, i.orgs)
	if err != nil {
		return nil, err
	}

	invitationID, err := strconv.ParseInt(resourceId.GetResource(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("baton-github: invalid invitation id")
	}

	var (
		isRemoved = false
		resp      *github.Response
	)

	for _, org := range orgs {
		resp, err = i.client.Organizations.CancelInvite(ctx, org, invitationID)
		if err == nil {
			isRemoved = true
			continue
		}
		if isNotFoundError(resp) {
			// Invitation is already gone (expired or previously cancelled).
			// Desired state is achieved, so treat as success.
			isRemoved = true
		}
	}

	if !isRemoved {
		return nil, fmt.Errorf("baton-github: failed to cancel invite")
	}

	restApiRateLimit, err := extractRateLimitData(resp)
	if err != nil {
		return nil, err
	}

	var annotations annotations.Annotations
	annotations.WithRateLimiting(restApiRateLimit)
	return annotations, nil
}

type createUserParams struct {
	org   string
	email *string
	login string // optional GitHub username
}

func getCreateUserParams(accountInfo *v2.AccountInfo) (*createUserParams, error) {
	pMap := accountInfo.Profile.AsMap()
	org, ok := pMap["org"].(string)
	if !ok || org == "" {
		return nil, fmt.Errorf("org is required")
	}

	e, emailExisted := pMap["email"].(string)
	if !emailExisted || e == "" {
		return nil, fmt.Errorf("email is required")
	}

	login, _ := pMap["github_username"].(string)

	return &createUserParams{
		org:   org,
		email: &e,
		login: login,
	}, nil
}

// lookupUser resolves a GitHub user resource. Tries login via Users.Get first
// (works regardless of email privacy), then falls back to email search.
func (i *invitationResourceType) lookupUser(ctx context.Context, login, email string) (*v2.Resource, error) {
	l := ctxzap.Extract(ctx)
	if login != "" {
		ghUser, _, err := i.client.Users.Get(ctx, login)
		if err == nil {
			userEmail := ghUser.GetEmail()
			if userEmail == "" {
				userEmail = email
			}
			return userResource(ctx, ghUser, userEmail, nil)
		}
		l.Debug("user lookup by login failed, falling back to email search",
			zap.String("login", login), zap.Error(err))
	}

	result, _, err := i.client.Search.Users(ctx, fmt.Sprintf(`"%s" in:email`, email), nil)
	if err != nil {
		return nil, fmt.Errorf("github-connector: failed to search users by email: %w", err)
	}
	if len(result.Users) == 0 {
		return nil, fmt.Errorf("github-connector: no user found with login %q or email %q", login, email)
	}
	return userResource(ctx, result.Users[0], email, nil)
}

// maxLookupPages limits pagination in invitation lookups to avoid excessive
// API calls and rate limit consumption for orgs with long invitation histories.
const maxLookupPages = 5

// lookupPendingInvitation searches pending org invitations matching by login or email.
// Returns (nil, nil) if no matching invitation is found.
func (i *invitationResourceType) lookupPendingInvitation(ctx context.Context, org, login, email string) (*v2.Resource, error) {
	opts := &github.ListOptions{PerPage: 100}
	for page := 0; page < maxLookupPages; page++ {
		invitations, resp, err := i.client.Organizations.ListPendingOrgInvitations(ctx, org, opts)
		if err != nil {
			return nil, fmt.Errorf("github-connector: failed to list pending invitations: %w", err)
		}
		for _, inv := range invitations {
			if invitationMatches(inv, login, email) {
				return invitationToUserResource(inv, invitationStatusPendingAcceptance)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return nil, nil
}

// lookupFailedInvitation searches failed/expired org invitations matching by login or email.
// Returns (nil, nil) if no matching invitation is found.
func (i *invitationResourceType) lookupFailedInvitation(ctx context.Context, org, login, email string) (*github.Invitation, error) {
	opts := &github.ListOptions{PerPage: 100}
	for page := 0; page < maxLookupPages; page++ {
		invitations, resp, err := i.client.Organizations.ListFailedOrgInvitations(ctx, org, opts)
		if err != nil {
			return nil, fmt.Errorf("github-connector: failed to list failed invitations: %w", err)
		}
		for _, inv := range invitations {
			if invitationMatches(inv, login, email) {
				return inv, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return nil, nil
}

// invitationMatches returns true if the invitation matches the given login or email.
func invitationMatches(inv *github.Invitation, login, email string) bool {
	if login != "" && strings.EqualFold(inv.GetLogin(), login) {
		return true
	}
	if email != "" && strings.EqualFold(inv.GetEmail(), email) {
		return true
	}
	return false
}

func isAlreadyOrgMemberError(err error, resp *github.Response) bool {
	return isGitHubValidationError(err, resp, "already a member", "already a part of")
}

func isAlreadyInvitedError(err error, resp *github.Response) bool {
	return isGitHubValidationError(err, resp, "already invited", "already been invited")
}

func isEMUOrgError(err error, resp *github.Response) bool {
	return isGitHubValidationError(err, resp, "managed by an enterprise", "enterprise managed")
}

// isGitHubValidationError returns true if the GitHub API response is a 422
// and the error message contains any of the given substrings (case-insensitive).
func isGitHubValidationError(err error, resp *github.Response, substrings ...string) bool {
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	var ghErr *github.ErrorResponse
	if !errors.As(err, &ghErr) {
		return false
	}
	for _, sub := range substrings {
		if containsLower(ghErr.Message, sub) {
			return true
		}
		for _, e := range ghErr.Errors {
			if containsLower(e.Message, sub) {
				return true
			}
		}
	}
	return false
}

func containsLower(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), substr)
}

type InvitationBuilderParams struct {
	client   *github.Client
	orgCache *orgNameCache
	orgs     []string
}

func InvitationBuilder(p InvitationBuilderParams) *invitationResourceType {
	return &invitationResourceType{
		client:   p.client,
		orgCache: p.orgCache,
		orgs:     p.orgs,
	}
}
