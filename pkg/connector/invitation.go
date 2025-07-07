package connector

import (
	"context"
	"fmt"
	"strconv"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"google.golang.org/grpc/codes"
)

func invitationToUserResource(invitation *github.Invitation) (*v2.Resource, error) {
	login := invitation.GetLogin()
	if login == "" {
		login = invitation.GetEmail()
	}

	ret, err := resource.NewUserResource(
		login,
		resourceTypeInvitation,
		invitation.GetID(),
		[]resource.UserTraitOption{
			resource.WithEmail(invitation.GetEmail(), true),
			resource.WithUserProfile(map[string]interface{}{
				"login":   login,
				"inviter": invitation.GetInviter().GetLogin(),
			}),
			resource.WithStatus(v2.UserTrait_Status_STATUS_UNSPECIFIED),
			resource.WithUserLogin(login),
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

func (i *invitationResourceType) List(ctx context.Context, parentID *v2.ResourceId, pt *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	var annotations annotations.Annotations
	if parentID == nil {
		return nil, "", nil, nil
	}

	bag, page, err := parsePageToken(pt.Token, &v2.ResourceId{ResourceType: resourceTypeInvitation.Id})
	if err != nil {
		return nil, "", nil, err
	}

	orgName, err := i.orgCache.GetOrgName(ctx, parentID)
	if err != nil {
		return nil, "", nil, err
	}
	invitations, resp, err := i.client.Organizations.ListPendingOrgInvitations(ctx, orgName, &github.ListOptions{
		Page:    page,
		PerPage: pt.Size,
	})
	if err != nil {
		if isNotFoundError(resp) {
			return nil, "", nil, uhttp.WrapErrors(codes.NotFound, fmt.Sprintf("org: %s not found", orgName))
		}
		return nil, "", nil, fmt.Errorf("github-connector: ListPendingOrgInvitatioins failed: %w", err)
	}

	restApiRateLimit, err := extractRateLimitData(resp)
	if err != nil {
		return nil, "", nil, err
	}

	nextPage, _, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, err
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, "", nil, err
	}

	invitationResources := make([]*v2.Resource, 0, len(invitations))
	for _, invitation := range invitations {
		ir, err := invitationToUserResource(invitation)
		if err != nil {
			return nil, "", nil, err
		}
		invitationResources = append(invitationResources, ir)
	}
	annotations.WithRateLimiting(restApiRateLimit)
	return invitationResources, pageToken, nil, nil
}

func (i *invitationResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

func (i *invitationResourceType) Grants(ctx context.Context, resource *v2.Resource, pToken *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	return nil, "", nil, nil
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
	credentialOptions *v2.CredentialOptions,
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
		return nil, nil, nil, fmt.Errorf("github-connectorv2: failed to invite user to org: %w", err)
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
		Resource: r,
	}, nil, nil, nil
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
