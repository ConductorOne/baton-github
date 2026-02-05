package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rType "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
)

const (
	teamRoleMember     = "member"
	teamRoleMaintainer = "maintainer"
)

var teamAccessLevels = []string{
	teamRoleMember,
	teamRoleMaintainer,
}

// teamResource creates a new connector resource for a GitHub Team. It is possible that the team has a parent resource.
func teamResource(team *github.Team, parentResourceID *v2.ResourceId) (*v2.Resource, error) {
	profile := map[string]interface{}{
		"members_count": team.GetMembersCount(),
		"repos_count":   team.GetReposCount(),
		// Store the org ID in the profile so that we can reference it when calculating grants
		"orgID": team.GetOrganization().GetID(),
	}

	ret, err := rType.NewGroupResource(
		team.GetName(),
		resourceTypeTeam,
		team.GetID(),
		[]rType.GroupTraitOption{rType.WithGroupProfile(profile)},
		rType.WithAnnotation(
			&v2.ExternalLink{Url: team.GetURL()},
			&v2.V1Identifier{Id: fmt.Sprintf("team:%d", team.GetID())},
		),
		rType.WithParentResourceID(parentResourceID),
	)
	if err != nil {
		return nil, err
	}

	return ret, nil
}

type teamResourceType struct {
	resourceType *v2.ResourceType
	client       *github.Client
	orgCache     *orgNameCache
}

func (o *teamResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *teamResourceType) List(ctx context.Context, parentID *v2.ResourceId, opts rType.SyncOpAttrs) ([]*v2.Resource, *rType.SyncOpResults, error) {
	if parentID == nil {
		return nil, &rType.SyncOpResults{}, nil
	}

	bag, page, err := parsePageToken(opts.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeTeam.Id})
	if err != nil {
		return nil, nil, err
	}

	listOpts := &github.ListOptions{
		Page:    page,
		PerPage: maxPageSize,
	}

	orgID, err := parseResourceToGitHub(parentID)
	if err != nil {
		return nil, nil, err
	}

	var rv []*v2.Resource

	orgName, err := o.orgCache.GetOrgName(ctx, opts.Session, parentID)
	if err != nil {
		return nil, nil, err
	}

	teams, resp, err := o.client.Teams.ListTeams(ctx, orgName, listOpts)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list teams")
	}

	nextPage, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, nil, err
	}

	for _, team := range teams {
		fullTeam, resp, err := o.client.Teams.GetTeamByID(ctx, orgID, team.GetID()) //nolint:staticcheck // TODO: migrate to GetTeamBySlug
		if err != nil {
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to get team details")
		}

		tr, err := teamResource(fullTeam, &v2.ResourceId{ResourceType: resourceTypeOrg.Id, Resource: fmt.Sprintf("%d", orgID)})
		if err != nil {
			return nil, nil, err
		}

		rv = append(rv, tr)
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, nil, err
	}

	return rv, &rType.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
}

func (o *teamResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ rType.SyncOpAttrs) ([]*v2.Entitlement, *rType.SyncOpResults, error) {
	rv := make([]*v2.Entitlement, 0, len(teamAccessLevels))
	for _, level := range teamAccessLevels {
		rv = append(
			rv,
			entitlement.NewPermissionEntitlement(
				resource,
				level,
				entitlement.WithAnnotation(
					&v2.V1Identifier{
						Id: fmt.Sprintf("team:%s:role:%s", resource.Id.Resource, level),
					},
				),
				entitlement.WithDisplayName(fmt.Sprintf("%s Team %s", resource.DisplayName, titleCase(level))),
				entitlement.WithDescription(fmt.Sprintf("Access to %s team in GitHub", resource.DisplayName)),
				entitlement.WithGrantableTo(resourceTypeUser),
			),
		)
	}

	return rv, &rType.SyncOpResults{}, nil
}

func (o *teamResourceType) Grants(ctx context.Context, resource *v2.Resource, opts rType.SyncOpAttrs) ([]*v2.Grant, *rType.SyncOpResults, error) {
	bag, page, err := parsePageToken(opts.PageToken.Token, resource.Id)
	if err != nil {
		return nil, nil, err
	}

	teamTrait, err := rType.GetGroupTrait(resource)
	if err != nil {
		return nil, nil, err
	}

	orgID, ok := rType.GetProfileInt64Value(teamTrait.Profile, "orgID")
	if !ok {
		return nil, nil, fmt.Errorf("error fetching orgID from team profile")
	}

	org, resp, err := o.client.Organizations.GetByID(ctx, orgID)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to get organization")
	}

	githubID, err := parseResourceToGitHub(resource.Id)
	if err != nil {
		return nil, nil, err
	}

	var (
		reqAnnos  annotations.Annotations
		pageToken string
		rv        = []*v2.Grant{}
	)
	switch rId := bag.ResourceTypeID(); rId {
	case resourceTypeTeam.Id:
		bag.Pop()
		bag.Push(pagination.PageState{
			ResourceTypeID: teamRoleMember,
		})
		bag.Push(pagination.PageState{
			ResourceTypeID: teamRoleMaintainer,
		})
	case teamRoleMember, teamRoleMaintainer:
		listOpts := github.TeamListTeamMembersOptions{
			ListOptions: github.ListOptions{
				Page:    page,
				PerPage: maxPageSize,
			},
			Role: rId,
		}

		users, resp, err := o.client.Teams.ListTeamMembersByID(ctx, org.GetID(), githubID, &listOpts)
		if err != nil {
			if isNotFoundError(resp) {
				return nil, nil, uhttp.WrapErrors(codes.NotFound, fmt.Sprintf("org: %d not found", org.GetID()))
			}
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list team members")
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
			rv = append(rv, grant.NewGrant(resource, rId, ur.Id,
				grant.WithAnnotation(&v2.V1Identifier{
					Id: fmt.Sprintf("team-grant:%s:%d:%s", resource.Id.Resource, user.GetID(), rId),
				}),
			))
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
	return rv, &rType.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
}

func (o *teamResourceType) Grant(ctx context.Context, principal *v2.Resource, entitlement *v2.Entitlement) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	if principal.Id.ResourceType != resourceTypeUser.Id {
		l.Warn(
			"github-connectorv2: only users can be granted team membership",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connectorv2: only users can be granted team membership")
	}

	teamId, err := strconv.ParseInt(entitlement.Resource.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	if entitlement.GetResource().GetParentResourceId() == nil {
		return nil, fmt.Errorf("github-connectorv2: parent resource is required to grant team membership")
	}

	// FIXME(jirwin): Now that we've flattened out the team hierarchy, we don't need to check the parent type.
	// Leaving this check here for backwards compatability with the old model.
	var orgId int64
	switch entitlement.Resource.ParentResourceId.ResourceType {
	case resourceTypeOrg.Id:
		var err error
		orgId, err = strconv.ParseInt(entitlement.Resource.ParentResourceId.Resource, 10, 64)
		if err != nil {
			return nil, err
		}
	case resourceTypeTeam.Id:
		groupTrait, err := rType.GetGroupTrait(entitlement.Resource)
		if err != nil {
			return nil, err
		}

		orgID, ok := rType.GetProfileInt64Value(groupTrait.Profile, "orgID")
		if !ok {
			return nil, fmt.Errorf("error fetching orgID from team profile")
		}

		orgId = orgID
	}

	userId, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	user, resp, err := o.client.Users.GetByID(ctx, userId)
	if err != nil {
		return nil, wrapGitHubError(err, resp, fmt.Sprintf("github-connector: failed to get user %d", userId))
	}

	enIDParts := strings.Split(entitlement.Id, ":")
	if len(enIDParts) != 3 {
		return nil, fmt.Errorf("github-connectorv2: invalid entitlement ID: %s", entitlement.Id)
	}
	permission := enIDParts[2]

	_, resp, er := o.client.Teams.AddTeamMembershipByID(
		ctx,
		orgId,
		teamId,
		user.GetLogin(),
		&github.TeamAddTeamMembershipOptions{Role: permission},
	)

	if er != nil {
		return nil, wrapGitHubError(er, resp, "github-connector: failed to add user to team")
	}

	return nil, nil
}

func (o *teamResourceType) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	entitlement := grant.Entitlement
	principal := grant.Principal

	if principal.Id.ResourceType != resourceTypeUser.Id {
		l.Warn(
			"github-connectorv2: only users can have team membership revoked",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connectorv2: only users can have team membership revoked")
	}

	teamId, err := strconv.ParseInt(entitlement.Resource.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	if entitlement.GetResource().GetParentResourceId() == nil {
		return nil, fmt.Errorf("github-connectorv2: parent resource is required to revoke team membership")
	}

	orgId, err := strconv.ParseInt(entitlement.Resource.ParentResourceId.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	userId, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	user, resp, err := o.client.Users.GetByID(ctx, userId)
	if err != nil {
		return nil, wrapGitHubError(err, resp, fmt.Sprintf("github-connector: failed to get user %d", userId))
	}
	resp, er := o.client.Teams.RemoveTeamMembershipByID(ctx, orgId, teamId, user.GetLogin())
	if er != nil {
		return nil, wrapGitHubError(er, resp, "github-connector: failed to revoke user team membership")
	}

	return nil, nil
}

func teamBuilder(client *github.Client, orgCache *orgNameCache) *teamResourceType {
	return &teamResourceType{
		resourceType: resourceTypeTeam,
		client:       client,
		orgCache:     orgCache,
	}
}
