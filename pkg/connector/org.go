package connector

import (
	"context"
	"fmt"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/google/go-github/v41/github"
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
}

func (o *orgResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *orgResourceType) List(
	ctx context.Context,
	_ *v2.ResourceId,
	pToken *pagination.Token,
) ([]*v2.Resource, string, annotations.Annotations, error) {
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
		membership, _, err := o.client.Organizations.GetOrgMembership(ctx, "", org.GetLogin())
		if err != nil {
			return nil, "", nil, err
		}

		// Only sync orgs that we are an admin for
		if strings.ToLower(membership.GetRole()) != orgRoleAdmin {
			continue
		}

		var annos annotations.Annotations
		annos.Append(&v2.ExternalLink{
			Url: org.GetHTMLURL(),
		})
		annos.Append(&v2.V1Identifier{
			Id: fmt.Sprintf("org:%d", org.GetID()),
		})
		annos.Append(&v2.ChildResourceType{ResourceTypeId: resourceTypeUser.Id})
		annos.Append(&v2.ChildResourceType{ResourceTypeId: resourceTypeTeam.Id})
		annos.Append(&v2.ChildResourceType{ResourceTypeId: resourceTypeRepository.Id})

		ret = append(ret, &v2.Resource{
			Id: &v2.ResourceId{
				ResourceType: resourceTypeOrg.Id,
				Resource:     org.GetLogin(),
			},
			DisplayName: org.GetLogin(),
			Annotations: annos,
		})
	}

	return ret, pageToken, reqAnnos, nil
}

func (o *orgResourceType) Entitlements(
	_ context.Context,
	resource *v2.Resource,
	_ *pagination.Token,
) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	rv := make([]*v2.Entitlement, 0, len(orgAccessLevels))
	for _, level := range orgAccessLevels {
		var annos annotations.Annotations
		annos.Append(&v2.V1Identifier{
			Id: fmt.Sprintf("org:%s:role:%s", resource.Id, level),
		})
		rv = append(rv, &v2.Entitlement{
			Id:          fmtResourceRole(resource.Id, level),
			Resource:    resource,
			DisplayName: fmt.Sprintf("%s Org %s", resource.DisplayName, titleCaser.String(level)),
			Description: fmt.Sprintf("Access to %s org in Github", resource.DisplayName),
			Annotations: annos,
			GrantableTo: []*v2.ResourceType{resourceTypeUser},
			Purpose:     v2.Entitlement_PURPOSE_VALUE_PERMISSION,
			Slug:        level,
		})
	}

	return rv, "", nil, nil
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

	orgName := getOrgName(resource.Id)

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

		ur, err := userResource(ctx, user)
		if err != nil {
			return nil, "", nil, err
		}

		roleName := strings.ToLower(membership.GetRole())
		switch roleName {
		case orgRoleAdmin, orgRoleMember:
			var annos annotations.Annotations
			annos.Append(&v2.V1Identifier{
				Id: fmt.Sprintf("org-grant:%s:%d:%s", resource.Id.Resource, user.GetID(), roleName),
			})
			rv = append(rv, &v2.Grant{
				Id: fmtResourceGrant(resource.Id, ur.Id, roleName),
				Entitlement: &v2.Entitlement{
					Id:       fmtResourceRole(resource.Id, roleName),
					Resource: resource,
				},
				Annotations: annos,
				Principal:   ur,
			})
		default:
			ctxzap.Extract(ctx).Warn("Unknown Github Role Name",
				zap.String("role_name", roleName),
				zap.String("github_username", user.GetLogin()),
			)
		}
	}

	return rv, pageToken, reqAnnos, nil
}

func orgBuilder(client *github.Client, orgs []string) *orgResourceType {
	orgMap := make(map[string]struct{})

	for _, o := range orgs {
		orgMap[o] = struct{}{}
	}

	return &orgResourceType{
		resourceType: resourceTypeOrg,
		orgs:         orgMap,
		client:       client,
	}
}
