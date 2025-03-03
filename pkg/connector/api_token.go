package connector

import (
	"context"
	"strconv"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/shurcooL/githubv4"
)

func apiTokenResource(ctx context.Context, token *github.PersonalAccessToken) (*v2.Resource, error) {
	userId := token.Owner.GetID()

	options := []resourceSdk.SecretTraitOption{}
	options = append(options,
		resourceSdk.WithSecretCreatedByID(&v2.ResourceId{
			ResourceType:  resourceTypeUser.Id,
			Resource:      strconv.FormatInt(userId, 10),
			BatonResource: false,
		}))

	if token.TokenLastUsedAt != nil {
		options = append(options, resourceSdk.WithSecretLastUsedAt(token.TokenLastUsedAt.Time))
	}

	if token.AccessGrantedAt != nil {
		options = append(options, resourceSdk.WithSecretCreatedAt(token.AccessGrantedAt.Time))
	}

	if token.TokenExpiresAt != nil {
		options = append(options, resourceSdk.WithSecretExpiresAt(token.TokenExpiresAt.Time))
	}
	rv, err := resourceSdk.NewSecretResource(
		token.GetTokenName(),
		resourceTypeApiToken,
		token.GetID(),
		options,
	)
	if err != nil {
		return nil, err
	}
	return rv, nil
}

type apiTokenResourceType struct {
	resourceType   *v2.ResourceType
	client         *github.Client
	graphClient    *githubv4.Client
	hasSAMLEnabled *bool
	orgCache       *orgNameCache
}

func (o *apiTokenResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *apiTokenResourceType) Entitlements(ctx context.Context, resource *v2.Resource, pToken *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	// API Token secrets do not have entitlements
	return nil, "", nil, nil
}

func (o *apiTokenResourceType) Grants(ctx context.Context, resource *v2.Resource, pToken *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	// API Token secrets do not have grants
	return nil, "", nil, nil
}

func (o *apiTokenResourceType) List(
	ctx context.Context,
	parentID *v2.ResourceId,
	pToken *pagination.Token,
) ([]*v2.Resource, string, annotations.Annotations, error) {
	var annotations annotations.Annotations
	if parentID == nil {
		return nil, "", nil, nil
	}

	bag, page, err := parsePageToken(pToken.Token, &v2.ResourceId{ResourceType: resourceTypeApiToken.Id})
	if err != nil {
		return nil, "", nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, parentID)
	if err != nil {
		return nil, "", nil, err
	}

	tokens, resp, err := o.client.Organizations.ListFineGrainedPersonalAccessTokens(ctx, orgName, &github.ListFineGrainedPATOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: pToken.Size,
		},
	})

	restApiRateLimit, err := extractRateLimitData(resp)
	if err != nil {
		return nil, "", nil, err
	}
	annotations.WithRateLimiting(restApiRateLimit)

	nextPage, _, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, err
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, "", nil, err
	}

	var rv []*v2.Resource
	for _, t := range tokens {
		resource, err := apiTokenResource(ctx, t)
		if err != nil {
			return nil, pageToken, annotations, err
		}
		rv = append(rv, resource)
	}

	return rv, pageToken, annotations, nil
}

func apiTokenBuilder(client *github.Client, hasSAMLEnabled *bool, orgCache *orgNameCache) *apiTokenResourceType {
	return &apiTokenResourceType{
		resourceType:   resourceTypeApiToken,
		client:         client,
		hasSAMLEnabled: hasSAMLEnabled,
		orgCache:       orgCache,
	}
}
