package connector

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
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

	return resourceSdk.NewRoleResource(
		role.Name,
		resourceTypeOrgRole,
		role.ID,
		[]resourceSdk.RoleTraitOption{
			resourceSdk.WithRoleProfile(profile),
		},
		resourceSdk.WithParentResourceID(org.Id),
		resourceSdk.WithAnnotation(
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
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	if parentID == nil {
		return nil, &resourceSdk.SyncOpResults{}, nil
	}

	orgName, err := o.orgCache.GetOrgName(ctx, opts.Session, parentID)
	if err != nil {
		return nil, nil, err
	}

	roles, resp, err := o.client.Organizations.ListRoles(ctx, orgName)
	if err != nil {
		// Handle permission errors gracefully
		if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
			// Return empty list with no error to indicate we skipped this resource
			return nil, &resourceSdk.SyncOpResults{}, nil
		}
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list organization roles")
	}

	var ret []*v2.Resource
	for _, role := range roles.CustomRepoRoles {
		roleResource, err := orgRoleResource(ctx, &OrganizationRole{
			ID:          role.GetID(),
			Name:        role.GetName(),
			Description: role.GetDescription(),
		}, &v2.Resource{Id: parentID})
		if err != nil {
			return nil, nil, err
		}
		ret = append(ret, roleResource)
	}

	return ret, &resourceSdk.SyncOpResults{}, nil
}

func (o *orgRoleResourceType) Entitlements(
	_ context.Context,
	resource *v2.Resource,
	_ resourceSdk.SyncOpAttrs,
) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	rv := make([]*v2.Entitlement, 0, 1)
	rv = append(rv, entitlement.NewAssignmentEntitlement(resource, "assigned",
		entitlement.WithDisplayName(resource.DisplayName),
		entitlement.WithDescription(fmt.Sprintf("Assignment to %s role in GitHub", resource.DisplayName)),
		entitlement.WithAnnotation(&v2.V1Identifier{
			Id: fmt.Sprintf("org_role:%s", resource.Id.Resource),
		}),
		entitlement.WithGrantableTo(resourceTypeUser),
	))

	return rv, &resourceSdk.SyncOpResults{}, nil
}

func (o *orgRoleResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	if resource == nil {
		return nil, &resourceSdk.SyncOpResults{}, nil
	}

	bag, page, err := parsePageToken(opts.PageToken.Token, resource.Id)
	if err != nil {
		return nil, nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, opts.Session, resource.ParentResourceId)
	if err != nil {
		return nil, nil, err
	}

	var rv []*v2.Grant
	var reqAnnos annotations.Annotations

	roleID, err := strconv.ParseInt(resource.Id.Resource, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid role ID: %w", err)
	}

	switch bag.ResourceTypeID() {
	case resourceTypeOrgRole.Id:
		bag.Pop()
		bag.Push(pagination.PageState{
			ResourceTypeID: resourceTypeUser.Id,
		})
		bag.Push(pagination.PageState{
			ResourceTypeID: resourceTypeTeam.Id,
		})
	case resourceTypeUser.Id:
		listOpts := &github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		}
		users, resp, err := o.client.Organizations.ListUsersAssignedToOrgRole(ctx, orgName, roleID, listOpts)
		if err != nil {
			if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
				pageToken, err := bag.NextToken("")
				if err != nil {
					return nil, nil, err
				}
				return rv, &resourceSdk.SyncOpResults{NextPageToken: pageToken}, nil
			}
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list users assigned to org role")
		}
		nextPage, respAnnos, err := parseResp(resp)
		if err != nil {
			return nil, nil, err
		}
		reqAnnos = respAnnos

		err = bag.Next(nextPage)
		if err != nil {
			return nil, nil, err
		}

		// Create regular grants for direct user assignments.
		for _, user := range users {
			userResource, err := userResource(ctx, user, user.GetEmail(), nil)
			if err != nil {
				return nil, nil, err
			}

			grant := grant.NewGrant(
				resource,
				"assigned",
				userResource.Id,
				grant.WithAnnotation(&v2.V1Identifier{
					Id: fmt.Sprintf("org-role:%s:%d:%d", resource.Id.Resource, user.GetID(), roleID),
				}),
			)
			grant.Principal = userResource
			rv = append(rv, grant)
		}
	case resourceTypeTeam.Id:
		listOpts := &github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		}
		teams, resp, err := o.client.Organizations.ListTeamsAssignedToOrgRole(ctx, orgName, roleID, listOpts)
		if err != nil {
			// Handle permission errors without erroring out. Some customers may not want to give us permissions to get org roles and members.
			if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
				// Return empty list with no error to indicate we skipped this resource
				pageToken, err := bag.NextToken("")
				if err != nil {
					return nil, nil, err
				}
				return nil, &resourceSdk.SyncOpResults{NextPageToken: pageToken}, nil
			}
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list teams assigned to org role")
		}

		nextPage, respAnnos, err := parseResp(resp)
		if err != nil {
			return nil, nil, err
		}
		reqAnnos = respAnnos

		err = bag.Next(nextPage)
		if err != nil {
			return nil, nil, err
		}

		// Create expandable grants for teams. To show inherited roles, we need to show the teams that have the role.
		for _, team := range teams {
			teamResource, err := teamResource(team, resource.ParentResourceId)
			if err != nil {
				return nil, nil, err
			}
			rv = append(rv, grant.NewGrant(
				resource,
				"assigned",
				teamResource.Id,
				grant.WithAnnotation(&v2.V1Identifier{
					Id: fmt.Sprintf("org-role-grant:%s:%d:%s", resource.Id.Resource, team.GetID(), "assigned"),
				},
					&v2.GrantExpandable{
						EntitlementIds: []string{
							entitlement.NewEntitlementID(teamResource, teamRoleMaintainer),
							entitlement.NewEntitlementID(teamResource, teamRoleMember),
						},
						Shallow: true,
					},
				),
			))
		}
	default:
		return nil, nil, fmt.Errorf("unexpected resource type while fetching grants for org role")
	}
	pageToken, err := bag.Marshal()
	if err != nil {
		return nil, nil, err
	}

	return rv, &resourceSdk.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
}

func (o *orgRoleResourceType) Grant(ctx context.Context, principal *v2.Resource, entitlement *v2.Entitlement) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

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

	orgName, err := o.orgCache.GetOrgNameFromRemoteServer(ctx, entitlement.Resource.ParentResourceId.GetResource())
	if err != nil {
		return nil, fmt.Errorf("failed to get org name: %w", err)
	}

	// First verify that the role exists
	req, err := o.client.NewRequest("GET", fmt.Sprintf("orgs/%s/organization-roles/%d", orgName, roleID), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := o.client.Do(ctx, req, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get role existence: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("role with ID %d not found in organization %s", roleID, orgName)
	}

	userID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid user ID: %w", err)
	}

	enIDParts := strings.Split(entitlement.Id, ":")
	if len(enIDParts) != 3 {
		return nil, fmt.Errorf("github-connectorv2: invalid entitlement ID: %s", entitlement.Id)
	}

	user, _, err := o.client.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	reqs, err := o.client.NewRequest("PUT", fmt.Sprintf("orgs/%s/organization-roles/users/%s/%d", orgName, user.GetLogin(), roleID), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	resp, err = o.client.Do(ctx, reqs, nil)
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

	orgName, err := o.orgCache.GetOrgNameFromRemoteServer(ctx, entitlement.Resource.ParentResourceId.GetResource())
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
