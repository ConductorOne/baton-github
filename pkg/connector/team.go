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

	teamPrivacySecret = "secret"
	teamPrivacyClosed = "closed"
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
	return nil
}

func (o *teamResourceType) registerCreateTeamAction(ctx context.Context, registry actions.ActionRegistry) error {
	return registry.Register(ctx, &v2.BatonActionSchema{
		Name:        "create",
		DisplayName: "Create Team",
		Description: "Create a new team in a GitHub organization",
		ActionType:  []v2.ActionType{v2.ActionType_ACTION_TYPE_RESOURCE_CREATE},
		Constraints: []*config.Constraint{
			{
				Kind:                config.ConstraintKind_CONSTRAINT_KIND_ALLOWED_OPTIONS,
				FieldNames:          []string{"privacy"},
				SecondaryFieldNames: []string{"parent"},
				AllowedOptionValues: []string{"closed"},
				Name:                "Privacy must be closed if parent is set",
				HelpText:            "Privacy must be closed if parent is set",
			},
		},
		Arguments: []*config.Field{
			{
				Name:        "name",
				DisplayName: "Team name",
				Description: "You'll use this name to mention this team in conversations.",
				Field:       &config.Field_StringField{},
				IsRequired:  true,
			},
			{
				Name:        "description",
				DisplayName: "Description",
				Description: "What is this team all about?",
				Field:       &config.Field_StringField{},
			},
			{
				Name:        "org",
				DisplayName: "Organization",
				Description: "The organization name.",
				Field: &config.Field_ResourceIdField{
					ResourceIdField: &config.ResourceIdField{
						Rules: &config.ResourceIDRules{
							AllowedResourceTypeIds: []string{resourceTypeOrg.Id},
						},
					},
				},
				IsRequired: true,
			},
			{
				Name:        "parent",
				DisplayName: "Parent team",
				Description: "The team to set as the parent team.",
				Field: &config.Field_ResourceIdField{
					ResourceIdField: &config.ResourceIdField{
						Rules: &config.ResourceIDRules{
							AllowedResourceTypeIds: []string{resourceTypeTeam.Id},
						},
					},
				},
			},
			{
				Name:        "privacy",
				DisplayName: "Privacy",
				Description: "The level of privacy this team should have.",
				Field: &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "secret", Name: "Secret is only visible to org owners and team members", DisplayName: "Secret"},
							{Value: "closed", Name: "Closed is visible to all org members. When parent team is set, this is the only allowed privacy level.", DisplayName: "Closed"},
						},
						DefaultValue: "closed",
					},
				},
			},
			{
				Name:        "notifications_enabled",
				DisplayName: "Team notifications",
				Description: "When enabled, team members receive notifications when the team is @mentioned.",
				Field: &config.Field_BoolField{
					BoolField: &config.BoolField{
						DefaultValue: true,
					},
				},
			},
			{
				Name:        "maintainers",
				DisplayName: "Team Maintainers",
				Description: "List of user resource IDs for organization members who will become team maintainers.",
				Field: &config.Field_ResourceIdSliceField{
					ResourceIdSliceField: &config.ResourceIdSliceField{
						Rules: &config.RepeatedResourceIdRules{
							AllowedResourceTypeIds: []string{resourceTypeUser.Id},
						},
					},
				},
			},
			{
				Name:        "repo_names",
				DisplayName: "Repositories",
				Description: "List of repository resource IDs to add the team to.",
				Field: &config.Field_ResourceIdSliceField{
					ResourceIdSliceField: &config.ResourceIdSliceField{
						Rules: &config.RepeatedResourceIdRules{
							AllowedResourceTypeIds: []string{resourceTypeRepository.Id},
						},
					},
				},
			},
		},
		ReturnTypes: []*config.Field{
			{Name: "success", Field: &config.Field_BoolField{}},
			{Name: "resource", Field: &config.Field_ResourceField{}},
			{Name: "entitlements", DisplayName: "Entitlements", Field: &config.Field_EntitlementSliceField{
				EntitlementSliceField: &config.EntitlementSliceField{},
			}},
			{Name: "grants", DisplayName: "Grants", Field: &config.Field_GrantSliceField{
				GrantSliceField: &config.GrantSliceField{},
			}},
		},
	}, o.handleCreateTeamAction)
}

func (o *teamResourceType) handleCreateTeamAction(ctx context.Context, args *structpb.Struct) (*structpb.Struct, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	// Extract required arguments using SDK helpers
	name, err := actions.RequireStringArg(args, "name")
	if err != nil {
		return nil, nil, err
	}

	parentResourceID, err := actions.RequireResourceIDArg(args, "org")
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

	// Check if this is a nested team (has parent)
	isNestedTeam := false
	if parentTeamResourceID, ok := actions.GetResourceIDArg(args, "parent"); ok {
		parentTeamID, err := strconv.ParseInt(parentTeamResourceID.Resource, 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid parent team ID: %w", err)
		}

		// Fetch the parent team to validate it's not a secret team
		// GitHub does not allow child teams under secret parent teams
		org, resp, err := o.client.Organizations.Get(ctx, orgName)
		if err != nil {
			return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to get organization %s", orgName))
		}
		parentTeam, resp, err := o.client.Teams.GetTeamByID(ctx, org.GetID(), parentTeamID) //nolint:staticcheck // TODO: migrate to GetTeamBySlug
		if err != nil {
			return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to get parent team %d", parentTeamID))
		}
		if parentTeam.GetPrivacy() == teamPrivacySecret {
			return nil, nil, fmt.Errorf("cannot create child team: parent team %q has privacy set to \"secret\"; GitHub does not allow child teams under secret parent teams", parentTeam.GetName())
		}

		newTeam.ParentTeamID = github.Ptr(parentTeamID)
		isNestedTeam = true
	}

	// Handle privacy with constraints based on team type:
	// - For non-nested teams: "secret" (default) or "closed"
	// - For nested/child teams: only "closed" is allowed (default: closed)
	if privacy, ok := actions.GetStringArg(args, "privacy"); ok && privacy != "" {
		switch {
		case isNestedTeam:
			// Nested teams can only be "closed"
			if privacy == teamPrivacySecret {
				l.Warn("github-connector: secret privacy not allowed for nested teams, using closed",
					zap.String("requested_privacy", privacy),
				)
			}
			newTeam.Privacy = github.Ptr(teamPrivacyClosed)
		case privacy == teamPrivacySecret || privacy == teamPrivacyClosed:
			// Non-nested teams can be "secret" or "closed"
			newTeam.Privacy = github.Ptr(privacy)
		default:
			// Invalid privacy value for non-nested team
			return nil, nil, fmt.Errorf("invalid privacy value: %q (must be \"secret\" or \"closed\")", privacy)
		}
	} else if isNestedTeam {
		// Default for nested teams is "closed"
		newTeam.Privacy = github.Ptr(teamPrivacyClosed)
	}
	// Note: Default for non-nested teams is "secret" (handled by GitHub API)

	if notificationsEnabled, ok := actions.GetBoolArg(args, "notifications_enabled"); ok {
		if notificationsEnabled {
			newTeam.NotificationSetting = github.Ptr("notifications_enabled")
		} else {
			newTeam.NotificationSetting = github.Ptr("notifications_disabled")
		}
	}

	if maintainerIDs, ok := actions.GetResourceIdListArg(args, "maintainers"); ok && len(maintainerIDs) > 0 {
		var maintainerLogins []string
		for _, rid := range maintainerIDs {
			userID, err := strconv.ParseInt(rid.Resource, 10, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid maintainer user ID %s: %w", rid.Resource, err)
			}
			user, resp, err := o.client.Users.GetByID(ctx, userID)
			if err != nil {
				return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to get user %d", userID))
			}
			maintainerLogins = append(maintainerLogins, user.GetLogin())
		}
		newTeam.Maintainers = maintainerLogins
	}

	if repoIDs, ok := actions.GetResourceIdListArg(args, "repo_names"); ok && len(repoIDs) > 0 {
		var repoFullNames []string
		for _, rid := range repoIDs {
			repoID, err := strconv.ParseInt(rid.Resource, 10, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid repository ID %s: %w", rid.Resource, err)
			}
			repo, resp, err := o.client.Repositories.GetByID(ctx, repoID)
			if err != nil {
				return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to get repository %d", repoID))
			}
			repoFullNames = append(repoFullNames, repo.GetFullName())
		}
		newTeam.RepoNames = repoFullNames
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
	teamRes, err := teamResource(createdTeam, parentResourceID)
	if err != nil {
		return nil, annos, fmt.Errorf("failed to create resource representation: %w", err)
	}

	// Generate entitlements for the newly created team (reuse existing method)
	entitlements, _, _, err := o.Entitlements(ctx, teamRes, nil)
	if err != nil {
		return nil, annos, fmt.Errorf("failed to generate entitlements: %w", err)
	}

	// Fetch grants for the newly created team by reusing the existing Grants method
	var grants []*v2.Grant
	pageToken := ""
	for {
		pToken := &pagination.Token{Token: pageToken}
		pageGrants, nextToken, _, err := o.Grants(ctx, teamRes, pToken)
		if err != nil {
			l.Warn("github-connector: failed to fetch grants for team", zap.Error(err))
			break
		}
		grants = append(grants, pageGrants...)
		if nextToken == "" {
			break
		}
		pageToken = nextToken
	}

	// Build return values using SDK helpers
	resourceRv, err := actions.NewResourceReturnField("resource", teamRes)
	if err != nil {
		return nil, annos, err
	}

	entitlementsRv, err := actions.NewEntitlementListReturnField("entitlements", entitlements)
	if err != nil {
		return nil, annos, err
	}

	grantsRv, err := actions.NewGrantListReturnField("grants", grants)
	if err != nil {
		return nil, annos, err
	}

	return actions.NewReturnValues(true, resourceRv, entitlementsRv, grantsRv), annos, nil
}

func teamBuilder(client *github.Client, orgCache *orgNameCache) *teamResourceType {
	return &teamResourceType{
		resourceType: resourceTypeTeam,
		client:       client,
		orgCache:     orgCache,
	}
}
