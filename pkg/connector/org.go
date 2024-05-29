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
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v62/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

const (
	orgRoleMember = "member"
	orgRoleAdmin  = "admin"
)

var orgAccessLevels = []string{
	orgRoleAdmin,
	orgRoleMember,
}

type orgResourceType struct {
	resourceType *v2.ResourceType
	client       *github.Client
	orgs         map[string]struct{}
	orgCache     *orgNameCache
}

func (o *orgResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *orgResourceType) List(
	ctx context.Context,
	parentResourceID *v2.ResourceId,
	pToken *pagination.Token,
) ([]*v2.Resource, string, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	bag, page, err := parsePageToken(pToken.Token, &v2.ResourceId{ResourceType: resourceTypeOrg.Id})
	if err != nil {
		return nil, "", nil, err
	}

	opts := &github.ListOptions{
		Page:    page,
		PerPage: pToken.Size,
	}

	orgs, resp, err := o.client.Organizations.List(ctx, "", opts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("github-connector: failed to fetch org: %w", err)
	}

	nextPage, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, err
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, "", nil, err
	}

	var ret []*v2.Resource
	for _, org := range orgs {
		if _, ok := o.orgs[org.GetLogin()]; !ok && len(o.orgs) > 0 {
			continue
		}
		membership, resp, err := o.client.Organizations.GetOrgMembership(ctx, "", org.GetLogin())
		if err != nil {
			if resp.StatusCode == http.StatusForbidden {
				l.Warn("insufficient access to list org membership, skipping org", zap.String("org", org.GetLogin()))
				continue
			}
			return nil, "", nil, err
		}

		// Only sync orgs that we are an admin for
		if strings.ToLower(membership.GetRole()) != orgRoleAdmin {
			continue
		}

		orgResource, err := resource.NewResource(
			org.GetLogin(),
			resourceTypeOrg,
			org.GetID(),
			resource.WithParentResourceID(parentResourceID),
			resource.WithAnnotation(
				&v2.ExternalLink{Url: org.GetHTMLURL()},
				&v2.V1Identifier{Id: fmt.Sprintf("org:%d", org.GetID())},
				&v2.ChildResourceType{ResourceTypeId: resourceTypeUser.Id},
				&v2.ChildResourceType{ResourceTypeId: resourceTypeTeam.Id},
				&v2.ChildResourceType{ResourceTypeId: resourceTypeRepository.Id},
			),
		)
		if err != nil {
			return nil, "", nil, err
		}

		ret = append(ret, orgResource)
	}

	return ret, pageToken, reqAnnos, nil
}

func (o *orgResourceType) Entitlements(
	_ context.Context,
	resource *v2.Resource,
	_ *pagination.Token,
) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	rv := make([]*v2.Entitlement, 0, len(orgAccessLevels))
	rv = append(rv, entitlement.NewAssignmentEntitlement(resource, orgRoleMember,
		entitlement.WithDisplayName(fmt.Sprintf("%s Org %s", resource.DisplayName, titleCase(orgRoleMember))),
		entitlement.WithDescription(fmt.Sprintf("Access to %s org in Github", resource.DisplayName)),
		entitlement.WithAnnotation(&v2.V1Identifier{
			Id: fmt.Sprintf("org:%s:role:%s", resource.Id.Resource, orgRoleMember),
		}),
		entitlement.WithGrantableTo(resourceTypeUser),
	))
	rv = append(rv, entitlement.NewPermissionEntitlement(resource, orgRoleAdmin,
		entitlement.WithDisplayName(fmt.Sprintf("%s Org %s", resource.DisplayName, titleCase(orgRoleAdmin))),
		entitlement.WithDescription(fmt.Sprintf("Access to %s org in Github", resource.DisplayName)),
		entitlement.WithAnnotation(&v2.V1Identifier{
			Id: fmt.Sprintf("org:%s:role:%s", resource.Id.Resource, orgRoleAdmin),
		}),
		entitlement.WithGrantableTo(resourceTypeUser),
	))

	return rv, "", nil, nil
}

func (o *orgResourceType) orgRoleGrant(roleName string, org *v2.Resource, principalID *v2.ResourceId, userID int64) *v2.Grant {
	return grant.NewGrant(org, roleName, principalID, grant.WithAnnotation(&v2.V1Identifier{
		Id: fmt.Sprintf("org-grant:%s:%d:%s", org.Id.Resource, userID, roleName),
	}))
}

func (o *orgResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	pToken *pagination.Token,
) ([]*v2.Grant, string, annotations.Annotations, error) {
	bag, page, err := parsePageToken(pToken.Token, resource.Id)
	if err != nil {
		return nil, "", nil, err
	}

	opts := github.ListMembersOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: pToken.Size,
		},
	}

	orgName, err := o.orgCache.GetOrgName(ctx, resource.Id)
	if err != nil {
		return nil, "", nil, err
	}

	users, resp, err := o.client.Organizations.ListMembers(ctx, orgName, &opts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("github-connectorv2: failed to list org members: %w", err)
	}

	nextPage, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, fmt.Errorf("github-connectorv2: failed to parse response: %w", err)
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, "", nil, err
	}

	var rv []*v2.Grant
	for _, user := range users {
		membership, _, err := o.client.Organizations.GetOrgMembership(ctx, user.GetLogin(), orgName)
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connectorv2: failed to get org memberships for user: %w", err)
		}
		if membership.GetState() == "pending" {
			continue
		}

		ur, err := userResource(ctx, user, user.GetEmail(), nil)
		if err != nil {
			return nil, "", nil, err
		}

		roleName := strings.ToLower(membership.GetRole())
		switch roleName {
		case orgRoleAdmin:
			rv = append(rv, o.orgRoleGrant(orgRoleAdmin, resource, ur.Id, user.GetID()))
			rv = append(rv, o.orgRoleGrant(orgRoleMember, resource, ur.Id, user.GetID()))

		case orgRoleMember:
			rv = append(rv, o.orgRoleGrant(orgRoleMember, resource, ur.Id, user.GetID()))

		default:
			ctxzap.Extract(ctx).Warn("Unknown Github Role Name",
				zap.String("role_name", roleName),
				zap.String("github_username", user.GetLogin()),
			)
		}
	}

	return rv, pageToken, reqAnnos, nil
}

func (o *orgResourceType) Grant(ctx context.Context, principal *v2.Resource, en *v2.Entitlement) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	if principal.Id.ResourceType != resourceTypeUser.Id {
		l.Error(
			"github-connectorv2: only users can be granted org admin",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connectorv2: only users can be granted org membership")
	}

	adminRoleID := entitlement.NewEntitlementID(en.Resource, orgRoleAdmin)
	memberRoleID := entitlement.NewEntitlementID(en.Resource, orgRoleMember)

	orgName, err := o.orgCache.GetOrgName(ctx, en.Resource.Id)
	if err != nil {
		return nil, err
	}

	principalID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	user, _, err := o.client.Users.GetByID(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to get user: %w", err)
	}

	requestedRole := ""
	switch en.Id {
	case adminRoleID:
		requestedRole = orgRoleAdmin
	case memberRoleID:
		requestedRole = "direct_member"
	default:
		return nil, fmt.Errorf("github-connectorv2: invalid entitlement id: %s", en.Id)
	}

	isMember, _, err := o.client.Organizations.IsMember(ctx, orgName, user.GetLogin())
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to get org membership: %w", err)
	}

	// If user isn't a member, invite them to the org with the requested role
	if !isMember {
		_, _, err = o.client.Organizations.CreateOrgInvitation(ctx, orgName, &github.CreateOrgInvitationOptions{
			InviteeID: user.ID,
			Role:      &requestedRole,
		})
		if err != nil {
			return nil, fmt.Errorf("github-connectorv2: failed to invite user to org: %w", err)
		}
		return nil, nil
	}

	if requestedRole == "direct_member" {

	}

	// If the user is a member, check to see what role they have
	membership, _, err := o.client.Organizations.GetOrgMembership(ctx, user.GetLogin(), orgName)
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to get org membership: %w", err)
	}

	// Skip if user is already an admin
	if membership.GetRole() == "admin" {
		l.Debug("githubv2-connector: user is already an admin of the org")
		return nil, nil
	}

	_, _, err = o.client.Organizations.EditOrgMembership(ctx, user.GetLogin(), orgName, &github.Membership{Role: github.String(orgRoleAdmin)})
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to make user an admin : %w", err)
	}

	return nil, nil
}

func (o *orgResourceType) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	en := grant.Entitlement
	principal := grant.Principal

	if principal.Id.ResourceType != resourceTypeUser.Id {
		l.Error(
			"github-connectorv2: org admin can only be revoked from users",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connectorv2: org admin can only be revoked from users")
	}

	if en.Id != entitlement.NewEntitlementID(en.Resource, orgRoleAdmin) {
		return nil, fmt.Errorf("github-connectorv2: invalid entitlement id: %s", en.Id)
	}

	orgName, err := o.orgCache.GetOrgName(ctx, en.Resource.Id)
	if err != nil {
		return nil, err
	}

	principalID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	user, _, err := o.client.Users.GetByID(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to get user: %w", err)
	}

	membership, _, err := o.client.Organizations.GetOrgMembership(ctx, user.GetLogin(), orgName)
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to get org membership: %w", err)
	}

	if membership.GetRole() == orgRoleMember {
		l.Debug("githubv2-connector: user is not an admin of the org")
		return nil, nil
	}

	if membership.GetState() != "active" {
		return nil, fmt.Errorf("github-connectorv2: user is not an active member of the org")
	}

	_, _, err = o.client.Organizations.EditOrgMembership(ctx, user.GetLogin(), orgName, &github.Membership{Role: github.String(orgRoleMember)})
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to revoke org admin from user : %w", err)
	}

	return nil, nil
}

func orgBuilder(client *github.Client, orgCache *orgNameCache, orgs []string) *orgResourceType {
	orgMap := make(map[string]struct{})

	for _, o := range orgs {
		orgMap[o] = struct{}{}
	}

	return &orgResourceType{
		resourceType: resourceTypeOrg,
		orgs:         orgMap,
		client:       client,
		orgCache:     orgCache,
	}
}
