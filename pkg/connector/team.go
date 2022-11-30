package connector

import (
	"context"
	"fmt"
	"strconv"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/sdk"
	"github.com/google/go-github/v41/github"
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
	var annos annotations.Annotations
	annos.Append(&v2.ExternalLink{
		// The GitHub client doesn't return HTMLURL() for some reason
		Url: team.GetURL(),
	})
	annos.Append(&v2.V1Identifier{
		Id: fmt.Sprintf("team:%d", team.GetID()),
	})

	profile := map[string]interface{}{
		"members_count": team.GetMembersCount(),
		"repos_count":   team.GetReposCount(),
		// Store the org ID in the profile so that we can reference it when calculating grants
		"orgID": team.GetOrganization().GetID(),
	}

	ret, err := sdk.NewGroupResource(team.GetName(), resourceTypeTeam, parentResourceID, team.GetID(), profile)
	if err != nil {
		return nil, err
	}

	ret.Annotations = append(ret.Annotations, annos...)

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

	teamParent := parentID

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
		bag.Pop()
		orgName, err := getOrgName(ctx, o.client, parentID)
		if err != nil {
			return nil, "", nil, err
		}

		teams, resp, err = o.client.Teams.ListTeams(ctx, orgName, opts)
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connector: failed to list teams: %w", err)
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
		teamParent = &v2.ResourceId{
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
		var annos annotations.Annotations
		annos.Append(&v2.V1Identifier{
			Id: fmt.Sprintf("team:%s:role:%s", resource.Id, level),
		})

		en := sdk.NewPermissionEntitlement(resource, level, resourceTypeUser)
		en.DisplayName = fmt.Sprintf("%s Team %s", resource.DisplayName, titleCaser.String(level))
		en.Description = fmt.Sprintf("Access to %s team in Github", resource.DisplayName)
		en.Annotations = annos
		rv = append(rv, en)
	}

	return rv, "", nil, nil
}

func (o *teamResourceType) Grants(ctx context.Context, resource *v2.Resource, pToken *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	bag, page, err := parsePageToken(pToken.Token, resource.Id)
	if err != nil {
		return nil, "", nil, err
	}

	teamTrait, err := sdk.GetGroupTrait(resource)
	if err != nil {
		return nil, "", nil, err
	}

	orgID, ok := sdk.GetProfileInt64Value(teamTrait.Profile, "orgID")
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
		var annos annotations.Annotations
		membership, _, err := o.client.Teams.GetTeamMembershipByID(ctx, org.GetID(), githubID, user.GetLogin())
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connectorv2: failed to get team membership for user: %w", err)
		}

		annos.Append(&v2.V1Identifier{
			Id: fmt.Sprintf("team-grant:%s:%d:%s", resource.Id.Resource, user.GetID(), membership.GetRole()),
		})

		ur, err := userResource(ctx, user)
		if err != nil {
			return nil, "", nil, err
		}

		grant := sdk.NewGrant(resource, membership.GetRole(), ur.Id)
		grant.Annotations = annos

		rv = append(rv, grant)
	}

	return rv, pageToken, reqAnnos, nil
}

func teamBuilder(client *github.Client) *teamResourceType {
	return &teamResourceType{
		resourceType: resourceTypeTeam,
		client:       client,
	}
}
