package connector

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v63/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

type OrganizationRole struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type OrganizationRoleResponse struct {
	TotalCount int                `json:"total_count"`
	Roles      []OrganizationRole `json:"roles"`
}

type OrganizationRoleTeam struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type orgRoleResourceType struct {
	resourceType *v2.ResourceType
	client       *github.Client
	orgCache     *orgNameCache
}

func orgRoleResource(
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

func (o *orgRoleResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *orgRoleResourceType) List(
	ctx context.Context,
	parentID *v2.ResourceId,
	pToken *pagination.Token,
) ([]*v2.Resource, string, annotations.Annotations, error) {
	if parentID == nil {
		return nil, "", nil, nil
	}

	bag, _, err := parsePageToken(pToken.Token, &v2.ResourceId{ResourceType: resourceTypeOrgRole.Id})
	if err != nil {
		return nil, "", nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, parentID)
	if err != nil {
		return nil, "", nil, err
	}

	roles, resp, err := o.client.Organizations.ListRoles(ctx, orgName)
	if err != nil {
		// Handle permission errors gracefully
		if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
			// Return empty list with no error to indicate we skipped this resource
			pageToken, err := bag.NextToken("")
			if err != nil {
				return nil, "", nil, err
			}
			return nil, pageToken, nil, nil
		}
		return nil, "", nil, fmt.Errorf("failed to list organization roles: %w", err)
	}

	var ret []*v2.Resource
	for _, role := range roles.CustomRepoRoles {
		roleResource, err := orgRoleResource(ctx, &OrganizationRole{
			ID:          role.GetID(),
			Name:        role.GetName(),
			Description: role.GetDescription(),
		}, &v2.Resource{Id: parentID})
		if err != nil {
			return nil, "", nil, err
		}
		ret = append(ret, roleResource)
	}

	pageToken, err := bag.NextToken("")
	if err != nil {
		return nil, "", nil, err
	}

	return ret, pageToken, nil, nil
}

func (o *orgRoleResourceType) Entitlements(
	_ context.Context,
	resource *v2.Resource,
	_ *pagination.Token,
) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	rv := make([]*v2.Entitlement, 0, 1)
	rv = append(rv, entitlement.NewAssignmentEntitlement(resource, "assigned",
		entitlement.WithDisplayName(resource.DisplayName),
		entitlement.WithDescription(fmt.Sprintf("Assignment to %s role in GitHub", resource.DisplayName)),
		entitlement.WithAnnotation(&v2.V1Identifier{
			Id: fmt.Sprintf("org_role:%s", resource.Id.Resource),
		}),
		entitlement.WithGrantableTo(resourceTypeUser),
	))

	return rv, "", nil, nil
}

func (o *orgRoleResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	pToken *pagination.Token,
) ([]*v2.Grant, string, annotations.Annotations, error) {
	if resource == nil {
		return nil, "", nil, nil
	}

	bag, _, err := parsePageToken(pToken.Token, &v2.ResourceId{ResourceType: resourceTypeOrgRole.Id})
	if err != nil {
		return nil, "", nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, resource.ParentResourceId)
	if err != nil {
		return nil, "", nil, err
	}

	roleID, err := strconv.ParseInt(resource.Id.Resource, 10, 64)
	if err != nil {
		return nil, "", nil, fmt.Errorf("invalid role ID: %w", err)
	}

	var ret []*v2.Grant

	// First, get teams with this role
	teams, resp, err := o.client.Organizations.ListTeamsAssignedToOrgRole(ctx, orgName, roleID, nil)
	if err != nil {
		// Handle permission errors without erroring out. Some customers may not want to give us permissions to get org roles and members.
		if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
			// Return empty list with no error to indicate we skipped this resource
			pageToken, err := bag.NextToken("")
			if err != nil {
				return nil, "", nil, err
			}
			return nil, pageToken, nil, nil
		}
		return nil, "", nil, fmt.Errorf("failed to list role teams: %w", err)
	}

	// Create expandable grants for teams. To show inherited roles, we need to show the teams that have the role.
	for _, team := range teams {
		teamResource, err := teamResource(team, resource.ParentResourceId)
		if err != nil {
			return nil, "", nil, err
		}

		grant := grant.NewGrant(
			resource,
			"assigned",
			teamResource.Id,
			grant.WithAnnotation(&v2.GrantExpandable{
				EntitlementIds: []string{fmt.Sprintf("team:%d:member", team.GetID())},
				Shallow:        true,
			}),
		)
		ret = append(ret, grant)
	}

	// Then, get direct user assignments
	users, resp, err := o.client.Organizations.ListUsersAssignedToOrgRole(ctx, orgName, roleID, nil)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
			pageToken, err := bag.NextToken("")
			if err != nil {
				return nil, "", nil, err
			}
			return ret, pageToken, nil, nil
		}
		return nil, "", nil, fmt.Errorf("failed to list role users: %w", err)
	}

	// Create regular grants for direct user assignments.
	for _, user := range users {
		userResource, err := userResource(ctx, user, user.GetEmail(), nil)
		if err != nil {
			return nil, "", nil, err
		}

		grant := grant.NewGrant(
			resource,
			"assigned",
			userResource.Id,
		)
		ret = append(ret, grant)
	}

	pageToken, err := bag.NextToken("")
	if err != nil {
		return nil, "", nil, err
	}

	return ret, pageToken, nil, nil
}

func (o *orgRoleResourceType) Grant(ctx context.Context, principal *v2.Resource, entitlement *v2.Entitlement) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	// Needs review, I copied this from the team grant function, but roles can be granted to teams as well, but we don't necessarily support that so wasn't sure if this was the intended behavior.
	if principal.Id.ResourceType != resourceTypeUser.Id {
		l.Warn(
			"github-connector: only users can be granted organization roles",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connector: only users can be granted organization roles")
	}

	roleID, err := strconv.ParseInt(entitlement.Resource.Id.Resource, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid role ID: %w", err)
	}

	orgName, err := o.orgCache.GetOrgName(ctx, entitlement.Resource.ParentResourceId)
	if err != nil {
		return nil, fmt.Errorf("failed to get org name: %w", err)
	}

	// First verify that the role exists
	roles, resp, err := o.client.Organizations.ListRoles(ctx, orgName)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
			return nil, fmt.Errorf("failed to verify role: organization not found or insufficient permissions")
		}
		return nil, fmt.Errorf("failed to verify role: %w", err)
	}

	// Check if the role exists
	roleExists := false
	for _, role := range roles.CustomRepoRoles {
		if role.GetID() == roleID {
			roleExists = true
			break
		}
	}

	if !roleExists {
		return nil, fmt.Errorf("role with ID %d not found in organization %s", roleID, orgName)
	}

	userID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid user ID: %w", err)
	}

	user, _, err := o.client.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	l.Info("attempting to assign role",
		zap.String("org", orgName),
		zap.Int64("role_id", roleID),
		zap.String("user", user.GetLogin()),
	)

	// Use the client's HTTP client to make the request with the correct URL format. Couldn't find a built in function for this. Cursor suggested this approach.
	url := fmt.Sprintf("orgs/%s/organization-roles/users/%s/%d", orgName, user.GetLogin(), roleID)
	req, err := o.client.NewRequest("PUT", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err = o.client.Do(ctx, req, nil)
	if err != nil {
		if resp != nil {
			l.Error("failed to assign role",
				zap.String("org", orgName),
				zap.Int64("role_id", roleID),
				zap.String("user", user.GetLogin()),
				zap.Int("status_code", resp.StatusCode),
				zap.String("status", resp.Status),
				zap.Error(err),
			)
		}
		return nil, fmt.Errorf("failed to assign role: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		l.Error("failed to assign role",
			zap.String("org", orgName),
			zap.Int64("role_id", roleID),
			zap.String("user", user.GetLogin()),
			zap.Int("status_code", resp.StatusCode),
			zap.String("status", resp.Status),
		)
		return nil, fmt.Errorf("failed to assign role: %s", resp.Status)
	}

	l.Info("successfully assigned role",
		zap.String("org", orgName),
		zap.Int64("role_id", roleID),
		zap.String("user", user.GetLogin()),
	)

	return nil, nil
}

func (o *orgRoleResourceType) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	entitlement := grant.Entitlement
	principal := grant.Principal

	// Needs review, I copied this from the team grant function, but roles can be granted to teams as well, but we don't necessarily support that so wasn't sure if this was the intended behavior.
	if principal.Id.ResourceType != resourceTypeUser.Id {
		l.Warn(
			"github-connector: only users can have organization roles revoked",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connector: only users can have organization roles revoked")
	}

	roleID, err := strconv.ParseInt(entitlement.Resource.Id.Resource, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid role ID: %w", err)
	}

	orgName, err := o.orgCache.GetOrgName(ctx, entitlement.Resource.ParentResourceId)
	if err != nil {
		return nil, fmt.Errorf("failed to get org name: %w", err)
	}

	userID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid user ID: %w", err)
	}

	user, _, err := o.client.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	url := fmt.Sprintf("orgs/%s/organization-roles/users/%s/%d", orgName, user.GetLogin(), roleID)
	req, err := o.client.NewRequest("DELETE", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := o.client.Do(ctx, req, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to revoke role: %w", err)
	}

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to revoke role: %s", resp.Status)
	}

	return nil, nil
}

func orgRoleBuilder(client *github.Client, orgCache *orgNameCache) *orgRoleResourceType {
	return &orgRoleResourceType{
		resourceType: resourceTypeOrgRole,
		client:       client,
		orgCache:     orgCache,
	}
}
