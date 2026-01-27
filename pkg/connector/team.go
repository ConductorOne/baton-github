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

// ResourceActions registers the resource actions for the team resource type.
// This implements the ResourceActionProvider interface.
func (o *teamResourceType) ResourceActions(ctx context.Context, registry actions.ActionRegistry) error {
	if err := o.registerCreateTeamAction(ctx, registry); err != nil {
		return err
	}
	if err := o.registerUpdateTeamAction(ctx, registry); err != nil {
		return err
	}
	if err := o.registerDeleteTeamAction(ctx, registry); err != nil {
		return err
	}
	return nil
}

func (o *teamResourceType) registerCreateTeamAction(ctx context.Context, registry actions.ActionRegistry) error {
	return registry.Register(ctx, &v2.BatonActionSchema{
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
				Field:       &config.Field_ResourceIdField{
					ResourceIdField: &config.ResourceIdField{
						Rules: &config.ResourceIDRules{
							AllowedResourceTypeIds: []string{resourceTypeOrg.Id},
						},
					},
				},
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
				Field:       &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "secret", DisplayName: "Secret (only visible to org owners and team members)"},
							{Value: "closed", DisplayName: "Closed (visible to all org members)"},
						},
					},
				},
			},
			{
				Name:        "notification_setting",
				DisplayName: "Notification Setting",
				Description: "The notification setting for the team",
				Field: &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "notifications_enabled", DisplayName: "Enabled"},
							{Value: "notifications_disabled", DisplayName: "Disabled"},
						},
					},
				},
			},
		},
		ReturnTypes: []*config.Field{
			{Name: "success", Field: &config.Field_BoolField{}},
			{Name: "resource", Field: &config.Field_ResourceField{}},
		},
	}, o.handleCreateTeamAction)
}

func (o *teamResourceType) registerDeleteTeamAction(ctx context.Context, registry actions.ActionRegistry) error {
	return registry.Register(ctx, &v2.BatonActionSchema{
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

func (o *teamResourceType) registerUpdateTeamAction(ctx context.Context, registry actions.ActionRegistry) error {
	return registry.Register(ctx, &v2.BatonActionSchema{
		Name:        "update",
		DisplayName: "Update Team",
		Description: "Update an existing team in a GitHub organization",
		ActionType:  []v2.ActionType{},
		Arguments: []*config.Field{
			{
				Name:        "resource",
				DisplayName: "Team Resource",
				Description: "The team resource to update",
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
			{
				Name:        "name",
				DisplayName: "Team Name",
				Description: "The new name of the team (leave empty to keep current)",
				Field:       &config.Field_StringField{},
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
				Description: "The privacy level of the team",
				Field: &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "secret", DisplayName: "Secret (only visible to org owners and team members)"},
							{Value: "closed", DisplayName: "Closed (visible to all org members)"},
						},
					},
				},
			},
			{
				Name:        "notification_setting",
				DisplayName: "Notification Setting",
				Description: "The notification setting for the team",
				Field: &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "notifications_enabled", DisplayName: "Enabled"},
							{Value: "notifications_disabled", DisplayName: "Disabled"},
						},
					},
				},
			},
		},
		ReturnTypes: []*config.Field{
			{Name: "success", Field: &config.Field_BoolField{}},
			{Name: "resource", Field: &config.Field_ResourceField{}},
		},
	}, o.handleUpdateTeamAction)
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

	// Get the organization name from the cache
	orgName, err := o.orgCache.GetOrgName(ctx, parentResourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get organization name: %w", err)
	}

	// Get the team to find its slug
	team, resp, err := o.client.Teams.GetTeamByID(ctx, orgID, teamID)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to get team %d", teamID))
	}

	teamSlug := team.GetSlug()

	l.Info("github-connector: deleting team via action",
		zap.Int64("team_id", teamID),
		zap.String("team_slug", teamSlug),
		zap.String("org_name", orgName),
	)

	// Delete the team using slug
	resp, err = o.client.Teams.DeleteTeamBySlug(ctx, orgName, teamSlug)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to delete team %s in org %s", teamSlug, orgName))
	}

	var annos annotations.Annotations
	if rateLimitData, err := extractRateLimitData(resp); err == nil {
		annos.WithRateLimiting(rateLimitData)
	}

	l.Info("github-connector: team deleted successfully via action",
		zap.Int64("team_id", teamID),
		zap.String("team_slug", teamSlug),
		zap.String("org_name", orgName),
	)

	return actions.NewReturnValues(true), annos, nil
}

func (o *teamResourceType) handleUpdateTeamAction(ctx context.Context, args *structpb.Struct) (*structpb.Struct, annotations.Annotations, error) {
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

	// Get the organization name from the cache
	orgName, err := o.orgCache.GetOrgName(ctx, parentResourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get organization name: %w", err)
	}

	// Get the team to find its slug
	team, resp, err := o.client.Teams.GetTeamByID(ctx, orgID, teamID)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to get team %d", teamID))
	}

	teamSlug := team.GetSlug()

	l.Info("github-connector: updating team via action",
		zap.Int64("team_id", teamID),
		zap.String("team_slug", teamSlug),
		zap.String("org_name", orgName),
	)

	// Build the NewTeam update request
	// Note: GitHub API uses NewTeam for both create and edit operations
	updateTeam := github.NewTeam{}

	// Track if any updates were provided
	hasUpdates := false

	// Extract optional fields using SDK helpers
	if name, ok := actions.GetStringArg(args, "name"); ok && name != "" {
		updateTeam.Name = name
		hasUpdates = true
	}

	if description, ok := actions.GetStringArg(args, "description"); ok {
		updateTeam.Description = github.Ptr(description)
		hasUpdates = true
	}

	if privacy, ok := actions.GetStringArg(args, "privacy"); ok && privacy != "" {
		if privacy == "secret" || privacy == "closed" {
			updateTeam.Privacy = github.Ptr(privacy)
			hasUpdates = true
		} else {
			l.Warn("github-connector: invalid privacy value, ignoring",
				zap.String("provided_privacy", privacy),
			)
		}
	}

	if notificationSetting, ok := actions.GetStringArg(args, "notification_setting"); ok && notificationSetting != "" {
		if notificationSetting == "notifications_enabled" || notificationSetting == "notifications_disabled" {
			updateTeam.NotificationSetting = github.Ptr(notificationSetting)
			hasUpdates = true
		} else {
			l.Warn("github-connector: invalid notification_setting value, ignoring",
				zap.String("provided_notification_setting", notificationSetting),
			)
		}
	}

	if parentTeamID, ok := actions.GetIntArg(args, "parent_team_id"); ok {
		if parentTeamID > 0 {
			updateTeam.ParentTeamID = github.Ptr(parentTeamID)
			hasUpdates = true
		}
		// Note: Setting to 0 would remove the parent, but GitHub API requires omitting the field entirely
	}

	if !hasUpdates {
		return nil, nil, fmt.Errorf("no update fields provided")
	}

	// Update the team via GitHub API using slug
	updatedTeam, resp, err := o.client.Teams.EditTeamBySlug(ctx, orgName, teamSlug, updateTeam, false)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to update team %s in org %s", teamSlug, orgName))
	}

	// Extract rate limit data for annotations
	var annos annotations.Annotations
	if rateLimitData, err := extractRateLimitData(resp); err == nil {
		annos.WithRateLimiting(rateLimitData)
	}

	l.Info("github-connector: team updated successfully via action",
		zap.Int64("team_id", updatedTeam.GetID()),
		zap.String("team_name", updatedTeam.GetName()),
		zap.String("team_slug", updatedTeam.GetSlug()),
	)

	// Create the resource representation of the updated team
	resource, err := teamResource(updatedTeam, parentResourceID)
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

func teamBuilder(client *github.Client, orgCache *orgNameCache) *teamResourceType {
	return &teamResourceType{
		resourceType: resourceTypeTeam,
		client:       client,
		orgCache:     orgCache,
	}
}
