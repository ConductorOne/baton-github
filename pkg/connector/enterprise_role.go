package connector

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/conductorone/baton-github/pkg/customclient"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"google.golang.org/grpc/codes"
)

type enterpriseRoleResourceType struct {
	resourceType   *v2.ResourceType
	client         *github.Client
	customClient   *customclient.Client
	enterprises    []string
	roleUsersCache map[string][]string
	mu             *sync.Mutex
}

func (o *enterpriseRoleResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
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
		page := 0
		continuePagination := true
		for continuePagination {
			consumedLicenses, _, err := o.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise, page)
			if err != nil {
				return uhttp.WrapErrors(codes.PermissionDenied, fmt.Sprintf("baton-github: error listing enterprise consumed licenses for %s", enterprise), err)
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
	rv := []*v2.Entitlement{}
	rv = append(rv, entitlement.NewAssignmentEntitlement(resource, "assigned",
		entitlement.WithDisplayName(resource.DisplayName),
		entitlement.WithDescription(fmt.Sprintf("Assignment to %s enterprise role in GitHub", resource.DisplayName)),
		entitlement.WithAnnotation(&v2.V1Identifier{
			Id: resource.Id.Resource,
		}),
		entitlement.WithGrantableTo(resourceTypeUser),
	))

	return rv, &resourceSdk.SyncOpResults{}, nil
}

func (o *enterpriseRoleResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
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
			"assigned",
			principalId,
		))
	}

	return ret, &resourceSdk.SyncOpResults{}, nil
}

func enterpriseRoleBuilder(client *github.Client, customClient *customclient.Client, enterprises []string) *enterpriseRoleResourceType {
	return &enterpriseRoleResourceType{
		resourceType:   resourceTypeEnterpriseRole,
		client:         client,
		customClient:   customClient,
		enterprises:    enterprises,
		roleUsersCache: make(map[string][]string),
		mu:             &sync.Mutex{},
	}
}
