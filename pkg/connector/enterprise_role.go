package connector

import (
	"context"
	"fmt"
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
)

type enterpriseRoleResourceType struct {
	resourceType   *v2.ResourceType
	client         *github.Client
	customClient   *customclient.Client
	enterprises    []string
	userRolesCache map[string][]string
	mu             *sync.Mutex
}

// func enterpriseRoleResource(
// 	ctx context.Context,
// 	role *OrganizationRole,
// 	org *v2.Resource,
// ) (*v2.Resource, error) {
// 	profile := map[string]interface{}{
// 		"description": role.Description,
// 	}
//
// 	return resource.NewRoleResource(
// 		role.Name,
// 		resourceTypeOrgRole,
// 		role.ID,
// 		[]resource.RoleTraitOption{
// 			resource.WithRoleProfile(profile),
// 		},
// 		resource.WithParentResourceID(org.Id),
// 		resource.WithAnnotation(
// 			&v2.V1Identifier{Id: fmt.Sprintf("org_role:%d", role.ID)},
// 		),
// 	)
// }

func (o *enterpriseRoleResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *enterpriseRoleResourceType) cacheRole(roleId string, userLogin string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.userRolesCache[userLogin]; !exists {
		o.userRolesCache[roleId] = []string{}
	}

	o.userRolesCache[roleId] = append(o.userRolesCache[userLogin], userLogin)
}

func (o *enterpriseRoleResourceType) fillCache() error {
	for _, enterprise := range o.enterprises {
		consumedLicenses, _, err := o.customClient.ListEnterpriseConsumedLicenses(context.Background(), enterprise)
		if err != nil {
			return fmt.Errorf("baton-github: error listing enterprise consumed licenses for %s: %w", enterprise, err)
		}

		for _, user := range consumedLicenses.Users {
			for _, role := range user.GitHubComEnterpriseRoles {
				roleId := fmt.Sprintf("%s:%s", enterprise, role)
				o.cacheRole(roleId, user.GitHubComLogin)
			}
		}
	}
	return nil
}

func (o *enterpriseRoleResourceType) getRolesCache() (map[string][]string, error) {
	if len(o.userRolesCache) == 0 {
		if err := o.fillCache(); err != nil {
			return nil, fmt.Errorf("baton-github: error caching user roles: %w", err)
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	return o.userRolesCache, nil
}

// func (o *enterpriseRoleResourceType) List(
// 	ctx context.Context,
// 	parentID *v2.ResourceId,
// 	pToken *pagination.Token,
// ) ([]*v2.Resource, string, annotations.Annotations, error) {
//
// 	var ret []*v2.Resource
// 	for _, enterprise := range o.enterprises {
// 		consumedLicenses, _, err := o.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise)
// 		if err != nil {
// 			return nil, "", nil, fmt.Errorf("baton-github: error listing enterprise consumed licenses for %s: %w", enterprise, err)
// 		}
//
// 		for _, user := range consumedLicenses.Users {
// 			for _, role := range user.GitHubComEnterpriseRoles {
// 				roleId := fmt.Sprintf("%s:%s", enterprise, role)
// 				o.cacheRole(roleId, user.GitHubComLogin)
//
// 				roleResource, err := resourceSdk.NewRoleResource(
// 					role,
// 					resourceTypeEnterpriseRole,
// 					roleId,
// 					[]resourceSdk.RoleTraitOption{},
// 				)
// 				if err != nil {
// 					return nil, "", nil, fmt.Errorf("baton-github: error creating role resource for %s in enterprise %s: %w", role, enterprise, err)
// 				}
// 				ret = append(ret, roleResource)
// 			}
// 		}
// 	}
//
// 	return ret, "", nil, nil
// }

func (o *enterpriseRoleResourceType) List(
	ctx context.Context,
	parentID *v2.ResourceId,
	pToken *pagination.Token,
) ([]*v2.Resource, string, annotations.Annotations, error) {
	var ret []*v2.Resource
	cache, err := o.getRolesCache()
	if err != nil {
		return nil, "", nil, fmt.Errorf("baton-github: error getting user roles cache: %w", err)
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
			return nil, "", nil, fmt.Errorf("baton-github: error creating role resource for %s in enterprise %s: %w", roleName, enterprise, err)
		}
		ret = append(ret, roleResource)
	}

	return ret, "", nil, nil
}

func (o *enterpriseRoleResourceType) Entitlements(
	_ context.Context,
	resource *v2.Resource,
	_ *pagination.Token,
) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	rv := []*v2.Entitlement{}
	rv = append(rv, entitlement.NewAssignmentEntitlement(resource, "assigned",
		entitlement.WithDisplayName(resource.DisplayName),
		entitlement.WithDescription(fmt.Sprintf("Assignment to %s enterprise role in GitHub", resource.DisplayName)),
		entitlement.WithAnnotation(&v2.V1Identifier{
			Id: fmt.Sprintf(resource.Id.Resource),
		}),
		entitlement.WithGrantableTo(resourceTypeUser),
	))

	return rv, "", nil, nil
}

func (o *enterpriseRoleResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	pToken *pagination.Token,
) ([]*v2.Grant, string, annotations.Annotations, error) {
	_, err := o.getRolesCache()
	if err != nil {
		return nil, "", nil, fmt.Errorf("baton-github: error getting enterprise role cache: %w", err)
	}

	cache, err := o.getRolesCache()
	if err != nil {
		return nil, "", nil, fmt.Errorf("baton-github: error getting user roles cache: %w", err)
	}

	// __AUTO_GENERATED_PRINT_VAR_START__
	fmt.Println(fmt.Sprintf("Grants resource.Id.Resource: %+v", resource.Id.Resource)) // __AUTO_GENERATED_PRINT_VAR_END__
	ret := []*v2.Grant{}
	for _, userLogin := range cache[resource.Id.Resource] {
		user, _, err := o.client.Users.Get(ctx, userLogin)
		if err != nil {
			return nil, "", nil, fmt.Errorf("baton-github: error getting user %s: %w", userLogin, err)
		}

		principalId, err := resourceSdk.NewResourceID(resourceTypeUser, *user.ID)
		if err != nil {
			return nil, "", nil, fmt.Errorf("baton-github: error creating resource ID for user %s: %w", userLogin, err)
		}

		ret = append(ret, grant.NewGrant(
			resource,
			"assigned",
			principalId,
		))
	}

	return ret, "", nil, nil
}

// func (o *enterpriseRoleResourceType) Grant(ctx context.Context, principal *v2.Resource, entitlement *v2.Entitlement) (annotations.Annotations, error) {
// 	return nil, nil
// }
//
// func (o *enterpriseRoleResourceType) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {
// 	return nil, nil
// }

func enterpriseRoleBuilder(client *github.Client, customClient *customclient.Client, enterprises []string) *enterpriseRoleResourceType {
	return &enterpriseRoleResourceType{
		resourceType:   resourceTypeEnterpriseRole,
		client:         client,
		customClient:   customClient,
		enterprises:    enterprises,
		userRolesCache: make(map[string][]string),
		mu:             &sync.Mutex{},
	}
}
