package connector

import (
	"context"
	"encoding/json"
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
)

type OrganizationRole struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
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

	// Use REST API directly since the client doesn't support these endpoints yet
	url := fmt.Sprintf("https://api.github.com/orgs/%s/organization-roles", orgName)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := o.client.Client().Do(req)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to list organization roles: %w", err)
	}
	defer resp.Body.Close()

	// Handle permission errors gracefully
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		// Return empty list with no error to indicate we skipped this resource
		pageToken, err := bag.NextToken("")
		if err != nil {
			return nil, "", nil, err
		}
		return nil, pageToken, nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", nil, fmt.Errorf("failed to list organization roles: %s", resp.Status)
	}

	var roleResp OrganizationRoleResponse
	if err := json.NewDecoder(resp.Body).Decode(&roleResp); err != nil {
		return nil, "", nil, fmt.Errorf("failed to decode response: %w", err)
	}

	var ret []*v2.Resource
	for _, role := range roleResp.Roles {
		roleResource, err := orgRoleResource(ctx, &role, &v2.Resource{Id: parentID})
		if err != nil {
			return nil, "", nil, err
		}
		ret = append(ret, roleResource)
	}

	// Since the API doesn't support pagination for roles yet, we'll return an empty token
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
	url := fmt.Sprintf("https://api.github.com/orgs/%s/organization-roles/%d/teams", orgName, roleID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := o.client.Client().Do(req)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to list role teams: %w", err)
	}
	defer resp.Body.Close()

	// Handle permission errors gracefully
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		// Return empty list with no error to indicate we skipped this resource
		pageToken, err := bag.NextToken("")
		if err != nil {
			return nil, "", nil, err
		}
		return nil, pageToken, nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", nil, fmt.Errorf("failed to list role teams: %s", resp.Status)
	}

	var teams []OrganizationRoleTeam
	if err := json.NewDecoder(resp.Body).Decode(&teams); err != nil {
		return nil, "", nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Create expandable grants for teams
	for _, team := range teams {
		teamResource, err := teamResource(&github.Team{ID: &team.ID, Name: &team.Name}, resource.ParentResourceId)
		if err != nil {
			return nil, "", nil, err
		}

		// Create an expandable grant for the team
		grant := grant.NewGrant(
			resource,
			"assigned",
			teamResource.Id,
			grant.WithAnnotation(&v2.GrantExpandable{
				EntitlementIds: []string{fmt.Sprintf("team:%d:member", team.ID)},
				Shallow:        true,
			}),
		)
		ret = append(ret, grant)
	}

	// Then, get direct user assignments
	url = fmt.Sprintf("https://api.github.com/orgs/%s/organization-roles/%d/users", orgName, roleID)
	req, err = http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err = o.client.Client().Do(req)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to list role users: %w", err)
	}
	defer resp.Body.Close()

	// Handle permission errors gracefully
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		// Return what we have so far (teams) with no error
		pageToken, err := bag.NextToken("")
		if err != nil {
			return nil, "", nil, err
		}
		return ret, pageToken, nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", nil, fmt.Errorf("failed to list role users: %s", resp.Status)
	}

	var users []*github.User
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, "", nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Create regular grants for direct user assignments
	for _, user := range users {
		grant := grant.NewGrant(
			resource,
			"assigned",
			&v2.ResourceId{
				ResourceType: resourceTypeUser.Id,
				Resource:     fmt.Sprintf("%d", user.GetID()),
			},
			grant.WithAnnotation(&v2.V1Identifier{
				Id: fmt.Sprintf("org_role_grant:%s:%d", resource.Id.Resource, user.GetID()),
			}),
		)
		ret = append(ret, grant)
	}

	// Since the API doesn't support pagination for role teams/users yet, we'll return an empty token
	pageToken, err := bag.NextToken("")
	if err != nil {
		return nil, "", nil, err
	}

	return ret, pageToken, nil, nil
}

func orgRoleBuilder(client *github.Client, orgCache *orgNameCache) *orgRoleResourceType {
	return &orgRoleResourceType{
		resourceType: resourceTypeOrgRole,
		client:       client,
		orgCache:     orgCache,
	}
}
