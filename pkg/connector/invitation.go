package connector

import (
	"context"
	"fmt"
	"strconv"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// invitationTTL is how long a GitHub organization invitation remains valid
// before it expires. GitHub org invitations expire 7 days after they are
// sent and the API does not return an explicit expiration timestamp, so we
// derive it from the invitation's created_at field.
const invitationTTL = 7 * 24 * time.Hour

func invitationToUserResource(invitation *github.Invitation) (*v2.Resource, error) {
	login := invitation.GetLogin()
	if login == "" {
		login = invitation.GetEmail()
	}

	ret, err := resourceSdk.NewUserResource(
		login,
		resourceTypeInvitation,
		invitation.GetID(),
		[]resourceSdk.UserTraitOption{
			resourceSdk.WithEmail(invitation.GetEmail(), true),
			resourceSdk.WithUserProfile(map[string]interface{}{
				"login":   login,
				"inviter": invitation.GetInviter().GetLogin(),
			}),
			resourceSdk.WithStatus(v2.UserTrait_Status_STATUS_UNSPECIFIED),
			resourceSdk.WithUserLogin(login),
		},
	)
	if err != nil {
		return nil, err
	}
	return ret, nil
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
	var annotations annotations.Annotations
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
	invitations, resp, err := i.client.Organizations.ListPendingOrgInvitations(ctx, orgName, &github.ListOptions{
		Page:    page,
		PerPage: opts.PageToken.Size,
	})
	if err != nil {
		if isNotFoundError(resp) {
			return nil, &resourceSdk.SyncOpResults{}, nil
		}
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list pending org invitations")
	}

	restApiRateLimit, err := extractRateLimitData(resp)
	if err != nil {
		return nil, nil, err
	}

	nextPage, _, err := parseResp(resp)
	if err != nil {
		return nil, nil, err
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, nil, err
	}

	invitationResources := make([]*v2.Resource, 0, len(invitations))
	for _, invitation := range invitations {
		ir, err := invitationToUserResource(invitation)
		if err != nil {
			return nil, nil, err
		}
		invitationResources = append(invitationResources, ir)
	}
	annotations.WithRateLimiting(restApiRateLimit)
	return invitationResources, &resourceSdk.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   annotations,
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
	params, err := getCreateUserParams(accountInfo)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("github-connectorv2: failed to get CreateUserParams: %w", err)
	}

	invitation, resp, err := i.client.Organizations.CreateOrgInvitation(ctx, params.org, &github.CreateOrgInvitationOptions{
		Email: params.email,
	})
	if err != nil {
		return nil, nil, nil, wrapGitHubError(err, resp, "github-connector: failed to create org invitation")
	}

	restApiRateLimit, err := extractRateLimitData(resp)
	if err != nil {
		return nil, nil, nil, err
	}

	var annotations annotations.Annotations
	annotations.WithRateLimiting(restApiRateLimit)

	r, err := invitationToUserResource(invitation)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("github-connectorv2: cannot create user resource: %w", err)
	}
	return &v2.CreateAccountResponse_SuccessResult{
		Resource:            r,
		InvitationExpiresAt: invitationExpiresAt(invitation),
	}, nil, annotations, nil
}

// invitationExpiresAt returns the wall-clock time at which the given GitHub
// organization invitation will expire, or nil if the invitation does not
// have a known creation time. GitHub does not return an explicit expiration
// in the API response; org invitations always expire invitationTTL after
// they are created.
func invitationExpiresAt(invitation *github.Invitation) *timestamppb.Timestamp {
	createdAt := invitation.GetCreatedAt()
	if createdAt.IsZero() {
		return nil
	}
	return timestamppb.New(createdAt.Add(invitationTTL))
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

	return &createUserParams{
		org:   org,
		email: &e,
	}, nil
}

type invitationBuilderParams struct {
	client   *github.Client
	orgCache *orgNameCache
	orgs     []string
}

func invitationBuilder(p invitationBuilderParams) *invitationResourceType {
	return &invitationResourceType{
		client:   p.client,
		orgCache: p.orgCache,
		orgs:     p.orgs,
	}
}
