package connector

import (
	"context"
	"fmt"
	"strconv"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rType "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v41/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
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
}

func (o *teamResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *teamResourceType) List(ctx context.Context, parentID *v2.ResourceId, pt *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	if parentID == nil {
		return nil, "", nil, nil
	}

	bag, page, err := parsePageToken(pt.Token, &v2.ResourceId{ResourceType: resourceTypeTeam.Id})
	if err != nil {
		return nil, "", nil, err
	}

	opts := &github.ListOptions{
		Page:    page,
		PerPage: pt.Size,
	}

	orgID, err := parseResourceToGithub(parentID)
	if err != nil {
		return nil, "", nil, err
	}

	var teams []*github.Team
	var resp *github.Response
	var rv []*v2.Resource

	switch bag.ResourceID() {
	// No resource ID set, so just list teams and push an action for each that we see
	case "":
		pageState := bag.Pop()
		orgName, err := getOrgName(ctx, o.client, parentID)
		if err != nil {
			return nil, "", nil, err
		}

		teams, resp, err = o.client.Teams.ListTeams(ctx, orgName, opts)
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connector: failed to list teams: %w", err)
		}

		if len(teams) == 0 {
			bag.Push(*pageState)
		}
		for _, t := range teams {
			bag.Push(pagination.PageState{
				ResourceTypeID: resourceTypeTeam.Id,
				ResourceID:     strconv.FormatInt(t.GetID(), 10),
			})
		}

	// We have a resource ID set, so we should check to see if the specific team has any children
	default:
		// Override the parent for the team because are looking at nested teams
		teamParent := &v2.ResourceId{
			ResourceType: bag.ResourceTypeID(),
			Resource:     bag.ResourceID(),
		}

		teamID, err := parseResourceToGithub(teamParent)
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connector: failed to convert parent resource ID to int64: %w", err)
		}

		teams, resp, err = o.client.Teams.ListChildTeamsByParentID(ctx, orgID, teamID, opts)
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connector: failed to list child teams: %w", err)
		}
	}

	nextPage, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, err
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, "", nil, err
	}

	for _, team := range teams {
		fullTeam, _, err := o.client.Teams.GetTeamByID(ctx, orgID, team.GetID())
		if err != nil {
			return nil, "", nil, err
		}

		teamParent := parentID
		if fullTeam.GetParent() != nil {
			teamParent, err = rType.NewResourceID(resourceTypeTeam, fullTeam.GetParent().GetID())
			if err != nil {
				return nil, "", nil, err
			}
		}

		tr, err := teamResource(fullTeam, teamParent)
		if err != nil {
			return nil, "", nil, err
		}

		rv = append(rv, tr)
	}

	return rv, pageToken, reqAnnos, nil
}

func (o *teamResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	rv := make([]*v2.Entitlement, 0, len(teamAccessLevels))
	for _, level := range teamAccessLevels {
		rv = append(rv, entitlement.NewPermissionEntitlement(resource, level,
			entitlement.WithAnnotation(
				&v2.V1Identifier{
					Id: fmt.Sprintf("team:%s:role:%s", resource.Id.Resource, level),
				},
			),
			entitlement.WithDisplayName(fmt.Sprintf("%s Team %s", resource.DisplayName, titleCase(level))),
			entitlement.WithDescription(fmt.Sprintf("Access to %s team in Github", resource.DisplayName)),
			entitlement.WithGrantableTo(resourceTypeUser),
		))
	}

	return rv, "", nil, nil
}

func (o *teamResourceType) Grants(ctx context.Context, resource *v2.Resource, pToken *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	bag, page, err := parsePageToken(pToken.Token, resource.Id)
	if err != nil {
		return nil, "", nil, err
	}

	teamTrait, err := rType.GetGroupTrait(resource)
	if err != nil {
		return nil, "", nil, err
	}

	orgID, ok := rType.GetProfileInt64Value(teamTrait.Profile, "orgID")
	if !ok {
		return nil, "", nil, fmt.Errorf("error fetching orgID from team profile")
	}

	org, _, err := o.client.Organizations.GetByID(ctx, orgID)
	if err != nil {
		return nil, "", nil, err
	}

	githubID, err := parseResourceToGithub(resource.Id)
	if err != nil {
		return nil, "", nil, err
	}

	opts := github.TeamListTeamMembersOptions{
		ListOptions: github.ListOptions{Page: page},
	}

	users, resp, err := o.client.Teams.ListTeamMembersByID(ctx, org.GetID(), githubID, &opts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("github-connectorv2: failed to fetch team members: %w", err)
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
		membership, _, err := o.client.Teams.GetTeamMembershipByID(ctx, org.GetID(), githubID, user.GetLogin())
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connectorv2: failed to get team membership for user: %w", err)
		}

		ur, err := userResource(ctx, user, user.GetEmail())
		if err != nil {
			return nil, "", nil, err
		}

		rv = append(rv, grant.NewGrant(resource, membership.GetRole(), ur.Id,
			grant.WithAnnotation(&v2.V1Identifier{
				Id: fmt.Sprintf("team-grant:%s:%d:%s", resource.Id.Resource, user.GetID(), membership.GetRole()),
			}),
		))
	}

	return rv, pageToken, reqAnnos, nil
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

	orgId, err := strconv.ParseInt(entitlement.Resource.ParentResourceId.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	userTrait, err := rType.GetUserTrait(principal)
	if err != nil {
		return nil, err
	}

	userLogin, ok := rType.GetProfileStringValue(userTrait.Profile, "login")
	if !ok {
		return nil, err
	}

	_, _, e := o.client.Teams.AddTeamMembershipByID(ctx, orgId, teamId, userLogin, &github.TeamAddTeamMembershipOptions{
		Role: entitlement.Slug,
	})
	if e != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to add user to a team: %w", e)
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

	userTrait, err := rType.GetUserTrait(principal)
	if err != nil {
		return nil, err
	}

	userLogin, ok := rType.GetProfileStringValue(userTrait.Profile, "login")
	if !ok {
		return nil, err
	}

	_, e := o.client.Teams.RemoveTeamMembershipByID(ctx, orgId, teamId, userLogin)
	if e != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to revoke user team membership: %w", e)
	}

	return nil, nil
}

func teamBuilder(client *github.Client) *teamResourceType {
	return &teamResourceType{
		resourceType: resourceTypeTeam,
		client:       client,
	}
}
