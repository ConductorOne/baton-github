package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	config "github.com/conductorone/baton-sdk/pb/c1/config/v1"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/actions"
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
		PerPage: maxPageSize,
	}

	orgID, err := parseResourceToGitHub(parentID)
	if err != nil {
		return nil, "", nil, err
	}

	var rv []*v2.Resource

	orgName, err := o.orgCache.GetOrgName(ctx, parentID)
	if err != nil {
		return nil, "", nil, err
	}

	teams, resp, err := o.client.Teams.ListTeams(ctx, orgName, opts)
	if err != nil {
		return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to list teams")
	}

	nextPage, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, err
	}

	for _, team := range teams {
		fullTeam, resp, err := o.client.Teams.GetTeamByID(ctx, orgID, team.GetID()) //nolint:staticcheck // TODO: migrate to GetTeamBySlug
		if err != nil {
			return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to get team details")
		}

		tr, err := teamResource(fullTeam, &v2.ResourceId{ResourceType: resourceTypeOrg.Id, Resource: fmt.Sprintf("%d", orgID)})
		if err != nil {
			return nil, "", nil, err
		}

		rv = append(rv, tr)
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, "", nil, err
	}

	return rv, pageToken, reqAnnos, nil
}

func (o *teamResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
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

	org, resp, err := o.client.Organizations.GetByID(ctx, orgID)
	if err != nil {
		return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to get organization")
	}

	githubID, err := parseResourceToGitHub(resource.Id)
	if err != nil {
		return nil, "", nil, err
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
		opts := github.TeamListTeamMembersOptions{
			ListOptions: github.ListOptions{
				Page:    page,
				PerPage: maxPageSize,
			},
			Role: rId,
		}

		users, resp, err := o.client.Teams.ListTeamMembersByID(ctx, org.GetID(), githubID, &opts)
		if err != nil {
			if isNotFoundError(resp) {
				return nil, "", nil, uhttp.WrapErrors(codes.NotFound, fmt.Sprintf("org: %d not found", org.GetID()))
			}
			return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to list team members")
		}

		var nextPage string
		nextPage, reqAnnos, err = parseResp(resp)
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connectorv2: failed to parse response: %w", err)
		}

		err = bag.Next(nextPage)
		if err != nil {
			return nil, "", nil, err
		}

		for _, user := range users {
			ur, err := userResource(ctx, user, user.GetEmail(), nil)
			if err != nil {
				return nil, "", nil, err
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
		return nil, "", nil, err
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

// Create creates a new team in a GitHub organization.
// The resource must have a parent resource ID that references the organization.
// The team name is taken from the resource's DisplayName field.
// Optional profile fields:
//   - description: string - Team description
//   - privacy: string - "secret" or "closed" (default: "secret")
//   - parent_team_id: int64 - ID of the parent team for nested teams
func (o *teamResourceType) Create(ctx context.Context, resource *v2.Resource) (*v2.Resource, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	if resource == nil {
		return nil, nil, fmt.Errorf("github-connector: resource cannot be nil")
	}

	if resource.Id == nil || resource.Id.ResourceType != resourceTypeTeam.Id {
		return nil, nil, fmt.Errorf("github-connector: invalid resource type for team creation")
	}

	// Get the parent org resource ID
	parentResourceID := resource.GetParentResourceId()
	if parentResourceID == nil {
		return nil, nil, fmt.Errorf("github-connector: parent organization resource ID is required to create a team")
	}

	if parentResourceID.ResourceType != resourceTypeOrg.Id {
		return nil, nil, fmt.Errorf("github-connector: parent resource must be an organization, got %s", parentResourceID.ResourceType)
	}

	// Get the organization name
	orgName, err := o.orgCache.GetOrgName(ctx, parentResourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("github-connector: failed to get organization name: %w", err)
	}

	// Get team name from display name
	teamName := resource.GetDisplayName()
	if teamName == "" {
		return nil, nil, fmt.Errorf("github-connector: team name (DisplayName) is required")
	}

	l.Info("github-connector: creating team",
		zap.String("team_name", teamName),
		zap.String("org_name", orgName),
	)

	// Build the NewTeam request
	newTeam := github.NewTeam{
		Name: teamName,
	}

	// Extract optional fields from the group trait profile if available
	groupTrait, err := rType.GetGroupTrait(resource)
	if err == nil && groupTrait != nil && groupTrait.Profile != nil {
		// Get description if provided
		if description, ok := rType.GetProfileStringValue(groupTrait.Profile, "description"); ok && description != "" {
			newTeam.Description = github.Ptr(description)
		}

		// Get privacy setting if provided ("secret" or "closed")
		if privacy, ok := rType.GetProfileStringValue(groupTrait.Profile, "privacy"); ok && privacy != "" {
			if privacy == "secret" || privacy == "closed" {
				newTeam.Privacy = github.Ptr(privacy)
			} else {
				l.Warn("github-connector: invalid privacy value, using default",
					zap.String("provided_privacy", privacy),
				)
			}
		}

		// Get parent team ID if provided (for nested teams)
		if parentTeamID, ok := rType.GetProfileInt64Value(groupTrait.Profile, "parent_team_id"); ok && parentTeamID > 0 {
			newTeam.ParentTeamID = github.Ptr(parentTeamID)
		}
	}

	// Create the team via GitHub API
	createdTeam, resp, err := o.client.Teams.CreateTeam(ctx, orgName, newTeam)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("github-connector: failed to create team %s in org %s", teamName, orgName))
	}

	// Extract rate limit data for annotations
	var annos annotations.Annotations
	if rateLimitData, err := extractRateLimitData(resp); err == nil {
		annos.WithRateLimiting(rateLimitData)
	}

	l.Info("github-connector: team created successfully",
		zap.String("team_name", createdTeam.GetName()),
		zap.Int64("team_id", createdTeam.GetID()),
		zap.String("team_slug", createdTeam.GetSlug()),
	)

	// Create the resource representation of the newly created team
	createdResource, err := teamResource(createdTeam, parentResourceID)
	if err != nil {
		return nil, annos, fmt.Errorf("github-connector: failed to create resource representation for team: %w", err)
	}

	return createdResource, annos, nil
}

// Delete deletes a team from a GitHub organization.
// The team is identified by its resource ID which contains the GitHub team ID.
func (o *teamResourceType) Delete(ctx context.Context, resourceId *v2.ResourceId) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	if resourceId == nil {
		return nil, fmt.Errorf("github-connector: resource ID cannot be nil")
	}

	if resourceId.ResourceType != resourceTypeTeam.Id {
		return nil, fmt.Errorf("github-connector: invalid resource type %s, expected %s", resourceId.ResourceType, resourceTypeTeam.Id)
	}

	// Parse the team ID from the resource
	teamID, err := strconv.ParseInt(resourceId.GetResource(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("github-connector: invalid team ID %s: %w", resourceId.GetResource(), err)
	}

	l.Info("github-connector: deleting team",
		zap.Int64("team_id", teamID),
	)

	// We need to find the org that this team belongs to.
	// We'll iterate through the organizations in the org cache.
	var annos annotations.Annotations
	var deleted bool
	var lastErr error
	var lastResp *github.Response

	// Use the org cache to get the list of organizations
	// We need to iterate through the configured organizations
	o.orgCache.RLock()
	orgIDs := make([]string, 0, len(o.orgCache.orgNames))
	for orgID := range o.orgCache.orgNames {
		orgIDs = append(orgIDs, orgID)
	}
	o.orgCache.RUnlock()

	for _, orgID := range orgIDs {
		orgIDInt, err := strconv.ParseInt(orgID, 10, 64)
		if err != nil {
			continue
		}

		// Try to get the team first to verify it exists in this org
		_, resp, err := o.client.Teams.GetTeamByID(ctx, orgIDInt, teamID)
		if err != nil {
			// Team doesn't exist in this org, continue to next
			if isNotFoundError(resp) {
				continue
			}
			lastErr = err
			lastResp = resp
			continue
		}

		// Team found in this org, delete it
		resp, err = o.client.Teams.DeleteTeamByID(ctx, orgIDInt, teamID)
		if err != nil {
			lastErr = err
			lastResp = resp
			continue
		}

		// Successfully deleted
		deleted = true
		if rateLimitData, err := extractRateLimitData(resp); err == nil {
			annos.WithRateLimiting(rateLimitData)
		}

		l.Info("github-connector: team deleted successfully",
			zap.Int64("team_id", teamID),
			zap.Int64("org_id", orgIDInt),
		)
		break
	}

	if !deleted {
		if lastErr != nil {
			return annos, wrapGitHubError(lastErr, lastResp, fmt.Sprintf("github-connector: failed to delete team %d", teamID))
		}
		return annos, fmt.Errorf("github-connector: team %d not found in any accessible organization", teamID)
	}

	return annos, nil
}

// ResourceActions registers the resource actions for the team resource type.
// This implements the ResourceActionProvider interface.
func (o *teamResourceType) ResourceActions(ctx context.Context, registry actions.ResourceTypeActionRegistry) error {
	if err := o.registerCreateTeamAction(ctx, registry); err != nil {
		return err
	}
	if err := o.registerDeleteTeamAction(ctx, registry); err != nil {
		return err
	}
	return nil
}

func (o *teamResourceType) registerCreateTeamAction(ctx context.Context, registry actions.ResourceTypeActionRegistry) error {
	return registry.Register(ctx, &v2.ResourceActionSchema{
		Name:        "create",
		DisplayName: "Create Team",
		Description: "Create a new team in a GitHub organization",
		ActionType:  []v2.ActionType{v2.ActionType_ACTION_TYPE_RESOURCE_CREATE},
		Arguments: []*config.Field{
			{
				Name:        "name",
				DisplayName: "Team Name",
				Description: "The name of the team to create",
				Field:       &config.Field_StringField{},
				IsRequired:  true,
			},
			{
				Name:        "parent",
				DisplayName: "Parent Organization",
				Description: "The organization to create the team in",
				Field:       &config.Field_ResourceIdField{},
				IsRequired:  true,
			},
			{
				Name:        "description",
				DisplayName: "Description",
				Description: "A description of the team",
				Field:       &config.Field_StringField{},
			},
			{
				Name:        "privacy",
				DisplayName: "Privacy",
				Description: "The privacy level: 'secret' or 'closed'",
				Field:       &config.Field_StringField{},
			},
		},
		ReturnTypes: []*config.Field{
			{Name: "success", Field: &config.Field_BoolField{}},
			{Name: "resource", Field: &config.Field_ResourceField{}},
		},
	}, o.handleCreateTeamAction)
}

func (o *teamResourceType) registerDeleteTeamAction(ctx context.Context, registry actions.ResourceTypeActionRegistry) error {
	return registry.Register(ctx, &v2.ResourceActionSchema{
		Name:        "delete",
		DisplayName: "Delete Team",
		Description: "Delete a team from a GitHub organization",
		ActionType:  []v2.ActionType{v2.ActionType_ACTION_TYPE_RESOURCE_DELETE},
		Arguments: []*config.Field{
			{
				Name:        "resource",
				DisplayName: "Team Resource",
				Description: "The team resource to delete",
				Field:       &config.Field_ResourceIdField{},
				IsRequired:  true,
			},
			{
				Name:        "parent",
				DisplayName: "Parent Organization",
				Description: "The organization the team belongs to",
				Field:       &config.Field_ResourceIdField{},
				IsRequired:  true,
			},
		},
		ReturnTypes: []*config.Field{
			{Name: "success", Field: &config.Field_BoolField{}},
		},
	}, o.handleDeleteTeamAction)
}

func (o *teamResourceType) handleCreateTeamAction(ctx context.Context, args *structpb.Struct) (*structpb.Struct, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	// Extract required arguments using SDK helpers
	name, err := actions.RequireStringArg(args, "name")
	if err != nil {
		return nil, nil, err
	}

	parentResourceID, err := actions.RequireResourceIDArg(args, "parent")
	if err != nil {
		return nil, nil, err
	}

	// Get the organization name from the parent resource ID
	orgName, err := o.orgCache.GetOrgName(ctx, parentResourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get organization name: %w", err)
	}

	l.Info("github-connector: creating team via action",
		zap.String("team_name", name),
		zap.String("org_name", orgName),
	)

	// Build the NewTeam request
	newTeam := github.NewTeam{
		Name: name,
	}

	// Extract optional fields using SDK helpers
	if description, ok := actions.GetStringArg(args, "description"); ok && description != "" {
		newTeam.Description = github.Ptr(description)
	}

	if privacy, ok := actions.GetStringArg(args, "privacy"); ok && privacy != "" {
		if privacy == "secret" || privacy == "closed" {
			newTeam.Privacy = github.Ptr(privacy)
		} else {
			l.Warn("github-connector: invalid privacy value, using default",
				zap.String("provided_privacy", privacy),
			)
		}
	}

	// Create the team via GitHub API
	createdTeam, resp, err := o.client.Teams.CreateTeam(ctx, orgName, newTeam)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to create team %s in org %s", name, orgName))
	}

	// Extract rate limit data for annotations
	var annos annotations.Annotations
	if rateLimitData, err := extractRateLimitData(resp); err == nil {
		annos.WithRateLimiting(rateLimitData)
	}

	l.Info("github-connector: team created successfully via action",
		zap.String("team_name", createdTeam.GetName()),
		zap.Int64("team_id", createdTeam.GetID()),
		zap.String("team_slug", createdTeam.GetSlug()),
	)

	// Create the resource representation of the newly created team
	resource, err := teamResource(createdTeam, parentResourceID)
	if err != nil {
		return nil, annos, fmt.Errorf("failed to create resource representation: %w", err)
	}

	// Build return values using SDK helpers
	resourceRv, err := actions.NewResourceReturnField("resource", resource)
	if err != nil {
		return nil, annos, err
	}

	return actions.NewReturnValues(true, resourceRv), annos, nil
}

func (o *teamResourceType) handleDeleteTeamAction(ctx context.Context, args *structpb.Struct) (*structpb.Struct, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	// Extract the team resource ID using SDK helper
	resourceID, err := actions.RequireResourceIDArg(args, "resource")
	if err != nil {
		return nil, nil, err
	}

	// Extract the parent org resource ID using SDK helper
	parentResourceID, err := actions.RequireResourceIDArg(args, "parent")
	if err != nil {
		return nil, nil, err
	}

	// Parse the team ID from the resource
	teamID, err := strconv.ParseInt(resourceID.Resource, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid team ID %s: %w", resourceID.Resource, err)
	}

	// Parse the org ID from the parent resource
	orgID, err := strconv.ParseInt(parentResourceID.Resource, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid org ID %s: %w", parentResourceID.Resource, err)
	}

	l.Info("github-connector: deleting team via action",
		zap.Int64("team_id", teamID),
		zap.Int64("org_id", orgID),
	)

	// Delete the team directly using the provided org ID from parent
	resp, err := o.client.Teams.DeleteTeamByID(ctx, orgID, teamID)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to delete team %d in org %d", teamID, orgID))
	}

	var annos annotations.Annotations
	if rateLimitData, err := extractRateLimitData(resp); err == nil {
		annos.WithRateLimiting(rateLimitData)
	}

	l.Info("github-connector: team deleted successfully via action",
		zap.Int64("team_id", teamID),
		zap.Int64("org_id", orgID),
	)

	return actions.NewReturnValues(true), annos, nil
}

func teamBuilder(client *github.Client, orgCache *orgNameCache) *teamResourceType {
	return &teamResourceType{
		resourceType: resourceTypeTeam,
		client:       client,
		orgCache:     orgCache,
	}
}
