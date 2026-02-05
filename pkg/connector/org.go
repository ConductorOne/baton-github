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
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

const (
	orgRoleMember       = "member"
	orgRoleDirectMember = "direct_member" // invite
	orgRoleAdmin        = "admin"
)

var orgAccessLevels = []string{
	orgRoleAdmin,
	orgRoleMember,
}

type orgResourceType struct {
	resourceType *v2.ResourceType
	client       *github.Client
	appClient    *github.Client
	orgs         map[string]struct{}
	orgCache     *orgNameCache
	syncSecrets  bool
}

func organizationResource(
	ctx context.Context,
	org *github.Organization,
	parentResourceID *v2.ResourceId,
	syncSecrets bool,
) (*v2.Resource, error) {
	annotations := []proto.Message{
		&v2.ExternalLink{Url: org.GetHTMLURL()},
		&v2.V1Identifier{Id: fmt.Sprintf("org:%d", org.GetID())},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeUser.Id},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeTeam.Id},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeRepository.Id},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeOrgRole.Id},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeInvitation.Id},
	}
	if syncSecrets {
		annotations = append(annotations, &v2.ChildResourceType{ResourceTypeId: resourceTypeApiToken.Id})
	}

	return resourceSdk.NewResource(
		org.GetLogin(),
		resourceTypeOrg,
		org.GetID(),
		resourceSdk.WithParentResourceID(parentResourceID),
		resourceSdk.WithAnnotation(
			annotations...,
		),
	)
}

func (o *orgResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *orgResourceType) List(
	ctx context.Context,
	parentResourceID *v2.ResourceId,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	if o.appClient != nil {
		orgResource, pageToken, anno, err := o.listOrganizationsFromAppInstallations(ctx, parentResourceID)
		if err != nil {
			return nil, nil, err
		}
		return []*v2.Resource{orgResource}, &resourceSdk.SyncOpResults{
			NextPageToken: pageToken,
			Annotations:   anno,
		}, nil
	}

	l := ctxzap.Extract(ctx)

	bag, page, err := parsePageToken(opts.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeOrg.Id})
	if err != nil {
		return nil, nil, err
	}

	listOpts := &github.ListOptions{
		Page:    page,
		PerPage: maxPageSize,
	}

	orgs, resp, err := o.client.Organizations.List(ctx, "", listOpts)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to fetch organizations")
	}

	nextPage, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, nil, err
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, nil, err
	}

	var ret []*v2.Resource
	for _, org := range orgs {
		if _, ok := o.orgs[org.GetLogin()]; !ok && len(o.orgs) > 0 {
			continue
		}
		membership, resp, err := o.client.Organizations.GetOrgMembership(ctx, "", org.GetLogin())
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusForbidden {
				l.Warn("insufficient access to list org membership, skipping org", zap.String("org", org.GetLogin()))
				continue
			}
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to get org membership")
		}

		// Only sync orgs that we are an admin for
		if strings.ToLower(membership.GetRole()) != orgRoleAdmin {
			continue
		}

		orgResource, err := organizationResource(ctx, org, parentResourceID, o.syncSecrets)
		if err != nil {
			return nil, nil, err
		}

		ret = append(ret, orgResource)
	}

	return ret, &resourceSdk.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
}

func (o *orgResourceType) Entitlements(
	_ context.Context,
	resource *v2.Resource,
	_ resourceSdk.SyncOpAttrs,
) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	rv := make([]*v2.Entitlement, 0, len(orgAccessLevels))
	rv = append(rv, entitlement.NewAssignmentEntitlement(resource, orgRoleMember,
		entitlement.WithDisplayName(fmt.Sprintf("%s Org %s", resource.DisplayName, titleCase(orgRoleMember))),
		entitlement.WithDescription(fmt.Sprintf("Access to %s org in GitHub", resource.DisplayName)),
		entitlement.WithAnnotation(&v2.V1Identifier{
			Id: fmt.Sprintf("org:%s:role:%s", resource.Id.Resource, orgRoleMember),
		}),
		entitlement.WithGrantableTo(resourceTypeUser),
	))
	rv = append(rv, entitlement.NewPermissionEntitlement(resource, orgRoleAdmin,
		entitlement.WithDisplayName(fmt.Sprintf("%s Org %s", resource.DisplayName, titleCase(orgRoleAdmin))),
		entitlement.WithDescription(fmt.Sprintf("Access to %s org in GitHub", resource.DisplayName)),
		entitlement.WithAnnotation(&v2.V1Identifier{
			Id: fmt.Sprintf("org:%s:role:%s", resource.Id.Resource, orgRoleAdmin),
		}),
		entitlement.WithGrantableTo(resourceTypeUser),
	))

	return rv, &resourceSdk.SyncOpResults{}, nil
}

func (o *orgResourceType) orgRoleGrant(roleName string, org *v2.Resource, principalID *v2.ResourceId, userID int64) *v2.Grant {
	return grant.NewGrant(org, roleName, principalID, grant.WithAnnotation(&v2.V1Identifier{
		Id: fmt.Sprintf("org-grant:%s:%d:%s", org.Id.Resource, userID, roleName),
	}))
}

func (o *orgResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	bag, page, err := parsePageToken(opts.PageToken.Token, resource.Id)
	if err != nil {
		return nil, nil, err
	}

	var (
		reqAnnos  annotations.Annotations
		pageToken string
		rv        = []*v2.Grant{}
	)

	switch rId := bag.ResourceTypeID(); rId {
	case resourceTypeOrg.Id:
		bag.Pop()
		bag.Push(pagination.PageState{
			ResourceTypeID: orgRoleAdmin,
		})
		bag.Push(pagination.PageState{
			ResourceTypeID: orgRoleMember,
		})
	case orgRoleAdmin, orgRoleMember:

		orgName, err := o.orgCache.GetOrgName(ctx, opts.Session, resource.Id)
		if err != nil {
			return nil, nil, err
		}
		listOpts := github.ListMembersOptions{
			Role: rId,
			ListOptions: github.ListOptions{
				Page:    page,
				PerPage: maxPageSize,
			},
		}
		users, resp, err := o.client.Organizations.ListMembers(ctx, orgName, &listOpts)
		if err != nil {
			if isNotFoundError(resp) {
				return nil, nil, uhttp.WrapErrors(codes.NotFound, fmt.Sprintf("org: %s not found", orgName))
			}
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list org members")
		}

		var nextPage string
		nextPage, reqAnnos, err = parseResp(resp)
		if err != nil {
			return nil, nil, fmt.Errorf("github-connectorv2: failed to parse response: %w", err)
		}

		err = bag.Next(nextPage)
		if err != nil {
			return nil, nil, err
		}

		for _, user := range users {
			ur, err := userResource(ctx, user, user.GetEmail(), nil)
			if err != nil {
				return nil, nil, err
			}

			if rId == orgRoleAdmin {
				rv = append(rv, o.orgRoleGrant(orgRoleMember, resource, ur.Id, user.GetID()))
			}
			rv = append(rv, o.orgRoleGrant(rId, resource, ur.Id, user.GetID()))
		}
	default:
		ctxzap.Extract(ctx).Warn("Unknown GitHub Role Name",
			zap.String("role_name", rId),
		)
	}

	pageToken, err = bag.Marshal()
	if err != nil {
		return nil, nil, err
	}
	return rv, &resourceSdk.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
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

	orgName, err := o.orgCache.GetOrgNameFromRemoteServer(ctx, en.Resource.Id.GetResource())
	if err != nil {
		return nil, err
	}

	principalID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	user, resp, err := o.client.Users.GetByID(ctx, principalID)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to get user")
	}

	requestedRole := ""
	switch en.Id {
	case adminRoleID:
		requestedRole = orgRoleAdmin
	case memberRoleID:
		requestedRole = orgRoleDirectMember
	default:
		return nil, fmt.Errorf("github-connectorv2: invalid entitlement id: %s", en.Id)
	}

	isMember, resp, err := o.client.Organizations.IsMember(ctx, orgName, user.GetLogin())
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to check org membership")
	}

	// TODO: check existing invitations. Duplicate invitations aren't allowed, so this will fail with 4xx from github.

	// If user isn't a member, invite them to the org with the requested role
	if !isMember {
		_, resp, err = o.client.Organizations.CreateOrgInvitation(ctx, orgName, &github.CreateOrgInvitationOptions{
			InviteeID: user.ID,
			Role:      &requestedRole,
		})
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to invite user to org")
		}
		return nil, nil
	}

	if requestedRole == orgRoleDirectMember {
		l.Debug("githubv2-connector: requested org membership but is already a member")
		return nil, nil
	}

	// If the user is a member, check to see what role they have
	membership, resp, err := o.client.Organizations.GetOrgMembership(ctx, user.GetLogin(), orgName)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to get org membership")
	}

	// Skip if user already has requested role
	if membership.GetRole() == orgRoleAdmin {
		l.Debug("githubv2-connector: user is already an admin of the org")
		return nil, nil
	}

	// User is a member but grant is for admin, so make them an admin.
	_, resp, err = o.client.Organizations.EditOrgMembership(ctx, user.GetLogin(), orgName, &github.Membership{Role: github.Ptr(orgRoleAdmin)})
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to make user an admin")
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

	adminRoleID := entitlement.NewEntitlementID(en.Resource, orgRoleAdmin)
	memberRoleID := entitlement.NewEntitlementID(en.Resource, orgRoleMember)

	if en.Id != adminRoleID && en.Id != memberRoleID {
		return nil, fmt.Errorf("github-connectorv2: invalid entitlement id: %s", en.Id)
	}

	orgName, err := o.orgCache.GetOrgNameFromRemoteServer(ctx, en.Resource.Id.GetResource())
	if err != nil {
		return nil, err
	}

	principalID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	user, resp, err := o.client.Users.GetByID(ctx, principalID)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to get user")
	}

	membership, resp, err := o.client.Organizations.GetOrgMembership(ctx, user.GetLogin(), orgName)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to get org membership")
	}

	if membership.GetState() != "active" {
		return nil, fmt.Errorf("github-connectorv2: user is not an active member of the org")
	}

	if en.Id == memberRoleID {
		resp, err = o.client.Organizations.RemoveOrgMembership(ctx, user.GetLogin(), orgName)
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to revoke org membership from user")
		}
		return nil, nil
	}

	_, resp, err = o.client.Organizations.EditOrgMembership(ctx, user.GetLogin(), orgName, &github.Membership{Role: github.Ptr(orgRoleMember)})
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to revoke org admin from user")
	}

	return nil, nil
}

func orgBuilder(client, appClient *github.Client, orgCache *orgNameCache, orgs []string, syncSecrets bool) *orgResourceType {
	orgMap := make(map[string]struct{})

	for _, o := range orgs {
		orgMap[o] = struct{}{}
	}

	return &orgResourceType{
		resourceType: resourceTypeOrg,
		orgs:         orgMap,
		client:       client,
		appClient:    appClient,
		orgCache:     orgCache,
		syncSecrets:  syncSecrets,
	}
}

func (o *orgResourceType) listOrganizationsFromAppInstallations(
	ctx context.Context,
	parentResourceID *v2.ResourceId,
) (*v2.Resource, string, annotations.Annotations, error) {
	if len(o.orgs) != 1 {
		return nil, "", nil, fmt.Errorf("github-connector: only one org should be specified")
	}

	var (
		org  *github.Organization
		resp *github.Response
		err  error
	)
	for orgName := range o.orgs {
		org, resp, err = o.client.Organizations.Get(ctx, orgName)
		if err != nil {
			return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to fetch organization")
		}
	}

	_, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, err
	}

	orgResource, err := organizationResource(ctx, org, parentResourceID, o.syncSecrets)
	if err != nil {
		return nil, "", nil, err
	}

	return orgResource, "", reqAnnos, nil
}
