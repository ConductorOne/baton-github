package connector

import (
	"context"
	"fmt"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/google/go-github/v41/github"
	"google.golang.org/protobuf/types/known/structpb"
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
func teamResource(ctx context.Context, orgName string, team *github.Team, parentResourceID *v2.ResourceId) (*v2.Resource, error) {
	t, err := teamTrait(ctx, team)
	if err != nil {
		return nil, err
	}

	var annos annotations.Annotations
	annos.Append(t)
	annos.Append(&v2.ExternalLink{
		// The GitHub client doesn't return HTMLURL() for some reason
		Url: team.GetURL(),
	})
	annos.Append(&v2.V1Identifier{
		Id: fmt.Sprintf("team:%d", team.GetID()),
	})
	annos.Append(&v2.ChildResourceType{ResourceTypeId: resourceTypeTeam.Id})

	return &v2.Resource{
		Id:               fmtResourceId(resourceTypeTeam.Id, orgName, team.GetID()),
		DisplayName:      team.GetName(),
		Annotations:      annos,
		ParentResourceId: parentResourceID,
	}, nil
}

// teamTrait creates a new GroupTrait for a github team.
func teamTrait(ctx context.Context, team *github.Team) (*v2.GroupTrait, error) {
	ret := &v2.GroupTrait{}
	profile, err := structpb.NewStruct(map[string]interface{}{
		"members_count": team.GetMembersCount(),
		"repos_count":   team.GetReposCount(),
	})
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to construct user profile for user trait: %w", err)
	}

	ret.Profile = profile

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

	orgName := getOrgName(parentID)

	org, _, err := o.client.Organizations.Get(ctx, orgName)
	if err != nil {
		return nil, "", nil, err
	}

	opts := &github.ListOptions{
		Page:    page,
		PerPage: pt.Size,
	}

	var teams []*github.Team
	var resp *github.Response

	switch parentID.ResourceType {
	case resourceTypeOrg.Id:
		teams, resp, err = o.client.Teams.ListTeams(ctx, orgName, opts)
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connector: failed to list teams: %w", err)
		}

	case resourceTypeTeam.Id:
		parentTeamID, err := parseResourceToGithub(parentID)
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connector: failed to convert parent resource ID to int64: %w", err)
		}

		teams, resp, err = o.client.Teams.ListChildTeamsByParentID(ctx, org.GetID(), parentTeamID, opts)
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

	rv := make([]*v2.Resource, 0, len(teams))
	for _, team := range teams {
		fullTeam, _, err := o.client.Teams.GetTeamByID(ctx, org.GetID(), team.GetID())
		if err != nil {
			return nil, "", nil, err
		}
		tr, err := teamResource(ctx, orgName, fullTeam, parentID)
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
		rv = append(rv, &v2.Entitlement{
			Id:          fmtResourceRole(resource.Id, level),
			Resource:    resource,
			DisplayName: fmt.Sprintf("%s Team %s", resource.DisplayName, titleCaser.String(level)),
			Description: fmt.Sprintf("Access to %s team in Github", resource.DisplayName),
			Annotations: annos,
			GrantableTo: []*v2.ResourceType{resourceTypeUser},
			Purpose:     v2.Entitlement_PURPOSE_VALUE_PERMISSION,
			Slug:        level,
		})
	}

	return rv, "", nil, nil
}

func (o *teamResourceType) Grants(ctx context.Context, resource *v2.Resource, pToken *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	bag, page, err := parsePageToken(pToken.Token, resource.Id)
	if err != nil {
		return nil, "", nil, err
	}

	orgName := getOrgName(resource.ParentResourceId)
	org, _, err := o.client.Organizations.Get(ctx, orgName)
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

		rv = append(rv, &v2.Grant{
			Entitlement: &v2.Entitlement{
				Id:       fmtResourceRole(resource.Id, membership.GetRole()),
				Resource: resource,
			},
			Id:          fmtResourceGrant(resource.Id, ur.Id, membership.GetRole()),
			Principal:   ur,
			Annotations: annos,
		})
	}

	return rv, pageToken, reqAnnos, nil
}

func teamBuilder(client *github.Client) *teamResourceType {
	return &teamResourceType{
		resourceType: resourceTypeTeam,
		client:       client,
	}
}
