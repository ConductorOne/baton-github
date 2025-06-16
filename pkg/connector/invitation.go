package connector

import (
	"context"
	"fmt"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/google/go-github/v69/github"
)

type invitationResourceType struct {
	client   *github.Client
	orgCache *orgNameCache
}

func (i *invitationResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return resourceTypeInvitation
}

func (i *invitationResourceType) List(ctx context.Context, parentID *v2.ResourceId, pt *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	var annotations annotations.Annotations
	if parentID == nil {
		return nil, "", nil, nil
	}

	bag, page, err := parsePageToken(pt.Token, &v2.ResourceId{ResourceType: resourceTypeUser.Id})
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

type invitationBuilderParams struct {
	client   *github.Client
	orgCache *orgNameCache
}

func invitationBuilder(p invitationBuilderParams) *invitationResourceType {
	return &invitationResourceType{
		client:   p.client,
		orgCache: p.orgCache,
	}
}
