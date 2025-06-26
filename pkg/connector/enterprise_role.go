package connector

import (
	"context"
	"fmt"
	"sync"

	"github.com/conductorone/baton-github/pkg/customclient"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
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

func enterpriseRoleResource(
	ctx context.Context,
	role *OrganizationRole,
	org *v2.Resource,
) (*v2.Resource, error) {
	profile := map[string]interface{}{
		"description": role.Description,
	}

	return resource.NewRoleResource(
		role.Name,
		resourceTypeOrgRole,
		role.ID,
		[]resource.RoleTraitOption{
			resource.WithRoleProfile(profile),
		},
		resource.WithParentResourceID(org.Id),
		resource.WithAnnotation(
			&v2.V1Identifier{Id: fmt.Sprintf("org_role:%d", role.ID)},
		),
	)
}

func (o *enterpriseRoleResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *enterpriseRoleResourceType) AddRoleCache(userLogin string, roleId string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.userRolesCache[userLogin]; !exists {
		o.userRolesCache[userLogin] = []string{}
	}

	o.userRolesCache[userLogin] = append(o.userRolesCache[userLogin], roleId)
}

func (o *enterpriseRoleResourceType) cacheUserRoles() error {
	for _, enterprise := range o.enterprises {
		consumedLicenses, _, err := o.customClient.ListEnterpriseConsumedLicenses(context.Background(), enterprise)
		if err != nil {
			return fmt.Errorf("baton-github: error listing enterprise consumed licenses for %s: %w", enterprise, err)
		}

		for _, user := range consumedLicenses.Users {
			for _, role := range user.GitHubComEnterpriseRoles {
				roleId := fmt.Sprintf("%s:%s", enterprise, role)
				o.AddRoleCache(user.GitHubComLogin, roleId)
			}
		}
	}
	return nil
}

func (o *enterpriseRoleResourceType) GetRoleCache(userLogin string) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.userRolesCache) == 0 {
		if err := o.cacheUserRoles(); err != nil {
			return nil, fmt.Errorf("baton-github: error caching user roles: %w", err)
		}
	}

	roles, exists := o.userRolesCache[userLogin]
	if !exists {
		return []string{}, nil
	}

	return roles, nil
}

func (o *enterpriseRoleResourceType) GetUserRolesCache() (map[string][]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.userRolesCache) == 0 {
		if err := o.cacheUserRoles(); err != nil {
			return nil, fmt.Errorf("baton-github: error caching user roles: %w", err)
		}
	}
	return o.userRolesCache, nil
}

func (o *enterpriseRoleResourceType) List(
	ctx context.Context,
	parentID *v2.ResourceId,
	pToken *pagination.Token,
) ([]*v2.Resource, string, annotations.Annotations, error) {
	if parentID == nil {
		return nil, "", nil, nil
	}

	var ret []*v2.Resource
	for _, enterprise := range o.enterprises {
		consumedLicenses, _, err := o.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise)
		if err != nil {
			return nil, "", nil, fmt.Errorf("baton-github: error listing enterprise consumed licenses for %s: %w", enterprise, err)
		}

		for _, user := range consumedLicenses.Users {
			for _, role := range user.GitHubComEnterpriseRoles {
				roleId := fmt.Sprintf("%s:%s", enterprise, role)
				o.AddRoleCache(user.GitHubComLogin, roleId)

				roleResource, err := resource.NewRoleResource(
					role,
					resourceTypeEnterpriseRole,
					roleId,
					[]resource.RoleTraitOption{},
				)
				if err != nil {
					return nil, "", nil, fmt.Errorf("baton-github: error creating role resource for %s in enterprise %s: %w", role, enterprise, err)
				}
				ret = append(ret, roleResource)
			}
		}
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
			Id: fmt.Sprintf("enterprise_role:%s", resource.Id.Resource),
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
	_, err := o.GetUserRolesCache()
	if err != nil {
		return nil, "", nil, fmt.Errorf("baton-github: error getting enterprise role cache: %w", err)
	}

	// get all users, use organizations.List and then organizations.Members

	return nil, "", nil, nil
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
