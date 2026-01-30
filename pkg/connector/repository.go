package connector

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	config "github.com/conductorone/baton-sdk/pb/c1/config/v1"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/actions"
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
	"google.golang.org/protobuf/types/known/structpb"
)

// outside collaborators are given one of these roles too.
const (
	repoPermissionPull     = "pull"
	repoPermissionTriage   = "triage"
	repoPermissionPush     = "push"
	repoPermissionMaintain = "maintain"
	repoPermissionAdmin    = "admin"
)

var repoAccessLevels = []string{
	repoPermissionPull,
	repoPermissionTriage,
	repoPermissionPush,
	repoPermissionMaintain,
	repoPermissionAdmin,
}

// repositoryResource returns a new connector resource for a GitHub repository.
func repositoryResource(ctx context.Context, repo *github.Repository, parentResourceID *v2.ResourceId) (*v2.Resource, error) {
	ret, err := resourceSdk.NewResource(
		repo.GetName(),
		resourceTypeRepository,
		repo.GetID(),
		resourceSdk.WithAnnotation(
			&v2.ExternalLink{Url: repo.GetHTMLURL()},
			&v2.V1Identifier{Id: fmt.Sprintf("repo:%d", repo.GetID())},
		),
		resourceSdk.WithParentResourceID(parentResourceID),
	)
	if err != nil {
		return nil, err
	}

	return ret, nil
}

type repositoryResourceType struct {
	resourceType             *v2.ResourceType
	client                   *github.Client
	orgCache                 *orgNameCache
	omitArchivedRepositories bool
}

func (o *repositoryResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *repositoryResourceType) List(ctx context.Context, parentID *v2.ResourceId, opts resourceSdk.SyncOpAttrs) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	if parentID == nil {
		return nil, &resourceSdk.SyncOpResults{}, nil
	}

	bag, page, err := parsePageToken(opts.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeRepository.Id})
	if err != nil {
		return nil, nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, opts.Session, parentID)
	if err != nil {
		return nil, nil, err
	}

	listOpts := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		},
	}

	repos, resp, err := o.client.Repositories.ListByOrg(ctx, orgName, listOpts)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list repositories")
	}

	nextPage, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, nil, err
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, nil, err
	}

	rv := make([]*v2.Resource, 0, len(repos))
	for _, repo := range repos {
		if o.omitArchivedRepositories && repo.GetArchived() {
			continue
		}
		rr, err := repositoryResource(ctx, repo, parentID)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, rr)
	}

	return rv, &resourceSdk.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
}

func (o *repositoryResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ resourceSdk.SyncOpAttrs) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	rv := make([]*v2.Entitlement, 0, len(repoAccessLevels))
	for _, level := range repoAccessLevels {
		rv = append(rv, entitlement.NewPermissionEntitlement(resource, level,
			entitlement.WithDisplayName(fmt.Sprintf("%s Repo %s", resource.DisplayName, titleCase(level))),
			entitlement.WithDescription(fmt.Sprintf("Access to %s repository in GitHub", resource.DisplayName)),
			entitlement.WithAnnotation(&v2.V1Identifier{
				Id: fmt.Sprintf("repo:%s:role:%s", resource.Id.Resource, level),
			}),
			entitlement.WithGrantableTo(resourceTypeUser, resourceTypeTeam),
		))
	}

	return rv, &resourceSdk.SyncOpResults{}, nil
}

func (o *repositoryResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	l := ctxzap.Extract(ctx)
	bag, page, err := parsePageToken(opts.PageToken.Token, resource.Id)
	if err != nil {
		return nil, nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, opts.Session, resource.ParentResourceId)
	if err != nil {
		return nil, nil, err
	}

	var rv []*v2.Grant
	var reqAnnos annotations.Annotations

	switch bag.ResourceTypeID() {
	case resourceTypeRepository.Id:
		bag.Pop()
		bag.Push(pagination.PageState{
			ResourceTypeID: resourceTypeUser.Id,
		})
		bag.Push(pagination.PageState{
			ResourceTypeID: resourceTypeTeam.Id,
		})

	case resourceTypeUser.Id:
		listOpts := &github.ListCollaboratorsOptions{
			Affiliation: "all",
			ListOptions: github.ListOptions{
				Page:    page,
				PerPage: maxPageSize,
			},
		}
		users, resp, err := o.client.Repositories.ListCollaborators(ctx, orgName, resource.DisplayName, listOpts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusForbidden {
				l.Warn("insufficient access to list collaborators", zap.String("repository", resource.DisplayName))
				pageToken, err := skipGrantsForResourceType(bag)
				if err != nil {
					return nil, nil, err
				}
				return nil, &resourceSdk.SyncOpResults{NextPageToken: pageToken}, nil
			}
			if isNotFoundError(resp) {
				return nil, nil, uhttp.WrapErrors(codes.NotFound, fmt.Sprintf("repo: %s not found", resource.DisplayName))
			}
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list collaborators")
		}

		nextPage, respAnnos, err := parseResp(resp)
		if err != nil {
			return nil, nil, err
		}
		reqAnnos = respAnnos

		err = bag.Next(nextPage)
		if err != nil {
			return nil, nil, err
		}

		for _, user := range users {
			for permission, hasPermission := range user.Permissions {
				if !hasPermission {
					continue
				}

				ur, err := userResource(ctx, user, user.GetEmail(), nil)
				if err != nil {
					return nil, nil, err
				}

				grant := grant.NewGrant(resource, permission, ur.Id, grant.WithAnnotation(&v2.V1Identifier{
					Id: fmt.Sprintf("repo-grant:%s:%d:%s", resource.Id.Resource, user.GetID(), permission),
				}))
				grant.Principal = ur
				rv = append(rv, grant)
			}
		}

	case resourceTypeTeam.Id:
		listOpts := &github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		}
		teams, resp, err := o.client.Repositories.ListTeams(ctx, orgName, resource.DisplayName, listOpts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusForbidden {
				l.Warn("insufficient access to list teams", zap.String("repository", resource.DisplayName))
				pageToken, err := skipGrantsForResourceType(bag)
				if err != nil {
					return nil, nil, err
				}
				return nil, &resourceSdk.SyncOpResults{NextPageToken: pageToken}, nil
			}

			if isNotFoundError(resp) {
				return nil, nil, uhttp.WrapErrors(codes.NotFound, fmt.Sprintf("repo: %s not found", resource.DisplayName))
			}

			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list repository teams")
		}

		nextPage, respAnnos, err := parseResp(resp)
		if err != nil {
			return nil, nil, err
		}
		reqAnnos = respAnnos

		err = bag.Next(nextPage)
		if err != nil {
			return nil, nil, err
		}

		for _, team := range teams {
			for permission, hasPermission := range team.Permissions {
				if !hasPermission {
					continue
				}

				tr, err := teamResource(team, resource.ParentResourceId)
				if err != nil {
					return nil, nil, err
				}

				rv = append(rv, grant.NewGrant(resource, permission, tr.Id, grant.WithAnnotation(
					&v2.V1Identifier{
						Id: fmt.Sprintf("repo-grant:%s:%d:%s", resource.Id.Resource, team.GetID(), permission),
					},
					&v2.GrantExpandable{
						EntitlementIds: []string{
							entitlement.NewEntitlementID(tr, teamRoleMaintainer),
							entitlement.NewEntitlementID(tr, teamRoleMember),
						},
						Shallow: true,
					},
				)))
			}
		}
	default:
		return nil, nil, fmt.Errorf("unexpected resource type while fetching grants for repo")
	}

	pageToken, err := bag.Marshal()
	if err != nil {
		return nil, nil, err
	}

	return rv, &resourceSdk.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
}

func (o *repositoryResourceType) Grant(ctx context.Context, principal *v2.Resource, en *v2.Entitlement) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	repoID, err := strconv.ParseInt(en.Resource.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	repo, resp, err := o.client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to get repository")
	}

	org := repo.GetOrganization()

	principalID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	enIDParts := strings.Split(en.Id, ":")
	if len(enIDParts) != 3 {
		return nil, fmt.Errorf("github-connectorv2: invalid entitlement ID: %s", en.Id)
	}
	permission := enIDParts[2]

	switch principal.Id.ResourceType {
	case resourceTypeUser.Id:
		user, resp, err := o.client.Users.GetByID(ctx, principalID)
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to get user")
		}

		_, resp, er := o.client.Repositories.AddCollaborator(
			ctx,
			repo.GetOwner().GetLogin(),
			repo.GetName(),
			user.GetLogin(),
			&github.RepositoryAddCollaboratorOptions{Permission: permission},
		)

		if er != nil {
			return nil, wrapGitHubError(er, resp, "github-connector: failed to add user to repository")
		}
	case resourceTypeTeam.Id:
		team, resp, err := o.client.Teams.GetTeamByID(ctx, org.GetID(), principalID) //nolint:staticcheck // TODO: migrate to GetTeamBySlug
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to get team")
		}

		resp, err = o.client.Teams.AddTeamRepoBySlug(ctx, org.GetLogin(), team.GetSlug(), repo.GetOwner().GetLogin(), repo.GetName(), &github.TeamAddTeamRepoOptions{
			Permission: permission,
		})
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to add team to repository")
		}
	default:
		l.Error(
			"github-connectorv2: only users and teams can be granted repository membership",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connectorv2: only users and teams can be granted team membership")
	}

	return nil, nil
}

func (o *repositoryResourceType) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	en := grant.Entitlement
	principal := grant.Principal

	repoID, err := strconv.ParseInt(en.Resource.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	repo, resp, err := o.client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to get repository")
	}

	org := repo.GetOrganization()

	principalID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	switch principal.Id.ResourceType {
	case resourceTypeUser.Id:
		user, resp, err := o.client.Users.GetByID(ctx, principalID)
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to get user")
		}

		resp, er := o.client.Repositories.RemoveCollaborator(ctx, repo.GetOwner().GetLogin(), repo.GetName(), user.GetLogin())
		if er != nil {
			return nil, wrapGitHubError(er, resp, "github-connector: failed to remove user from repository")
		}
	case resourceTypeTeam.Id:
		team, resp, err := o.client.Teams.GetTeamByID(ctx, org.GetID(), principalID) //nolint:staticcheck // TODO: migrate to GetTeamBySlug
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to get team")
		}

		resp, err = o.client.Teams.RemoveTeamRepoBySlug(ctx, org.GetLogin(), team.GetSlug(), repo.GetOwner().GetLogin(), repo.GetName())
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to remove team from repository")
		}
	default:
		l.Error(
			"github-connectorv2: only users and teams can have repository membership revoked",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connectorv2: only users and teams can be granted team membership")
	}

	return nil, nil
}

func repositoryBuilder(client *github.Client, orgCache *orgNameCache, omitArchivedRepositories bool) *repositoryResourceType {
	return &repositoryResourceType{
		resourceType:             resourceTypeRepository,
		client:                   client,
		orgCache:                 orgCache,
		omitArchivedRepositories: omitArchivedRepositories,
	}
}

func skipGrantsForResourceType(bag *pagination.Bag) (string, error) {
	err := bag.Next("")
	if err != nil {
		return "", err
	}
	pageToken, err := bag.Marshal()
	if err != nil {
		return "", err
	}
	return pageToken, nil
}

// ResourceActions registers the resource actions for the repository resource type.
// This implements the ResourceActionProvider interface.
func (o *repositoryResourceType) ResourceActions(ctx context.Context, registry actions.ActionRegistry) error {
	if err := o.registerCreateRepositoryAction(ctx, registry); err != nil {
		return err
	}
	return nil
}

func (o *repositoryResourceType) registerCreateRepositoryAction(ctx context.Context, registry actions.ActionRegistry) error {
	return registry.Register(ctx, &v2.BatonActionSchema{
		Name:        "create",
		DisplayName: "Create Repository",
		Description: "Create a new repository in a GitHub organization",
		ActionType:  []v2.ActionType{v2.ActionType_ACTION_TYPE_RESOURCE_CREATE},
		Arguments: []*config.Field{
			{
				Name:        "name",
				DisplayName: "Repository name",
				Description: "The name of the repository to create",
				Field:       &config.Field_StringField{},
				IsRequired:  true,
			},
			{
				Name:        "description",
				DisplayName: "Description",
				Description: "A description of the repository",
				Field:       &config.Field_StringField{},
			},
			{
				Name:        "org",
				DisplayName: "Organization",
				Description: "The organization to create the repository in",
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
				Name:        "visibility",
				DisplayName: "Visibility",
				Description: "The visibility level of the repository",
				Field: &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "public", DisplayName: "Public", Name: "Anyone on the internet can view this repository"},
							{Value: "private", DisplayName: "Private", Name: "You can choose who can see this repository"},
						},
						DefaultValue: "private",
					},
				},
			},
			{
				Name:        "add_readme",
				DisplayName: "Add README.md",
				Description: "Add a README.md file to the repository",
				Field: &config.Field_BoolField{
					BoolField: &config.BoolField{
						DefaultValue: true,
					},
				},
			},
			{
				Name:        "gitignore_template",
				DisplayName: "Gitignore Template",
				Description: "Gitignore template to apply",
				Field: &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "", DisplayName: "No .gitignore template"},
							{Value: "Go", DisplayName: "Go"},
							{Value: "Python", DisplayName: "Python"},
							{Value: "Node", DisplayName: "Node"},
							{Value: "Java", DisplayName: "Java"},
							{Value: "Ruby", DisplayName: "Ruby"},
							{Value: "Rust", DisplayName: "Rust"},
							{Value: "C++", DisplayName: "C++"},
							{Value: "C", DisplayName: "C"},
							{Value: "Swift", DisplayName: "Swift"},
							{Value: "Kotlin", DisplayName: "Kotlin"},
							{Value: "Scala", DisplayName: "Scala"},
							{Value: "Terraform", DisplayName: "Terraform"},
						},
					},
				},
			},
			{
				Name:        "license_template",
				DisplayName: "License Template",
				Description: "License template to apply",
				Field: &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "", DisplayName: "No license"},
							{Value: "mit", DisplayName: "MIT License"},
							{Value: "apache-2.0", DisplayName: "Apache License 2.0"},
							{Value: "gpl-3.0", DisplayName: "GNU GPLv3"},
							{Value: "gpl-2.0", DisplayName: "GNU GPLv2"},
							{Value: "lgpl-3.0", DisplayName: "GNU LGPLv3"},
							{Value: "bsd-3-clause", DisplayName: "BSD 3-Clause"},
							{Value: "bsd-2-clause", DisplayName: "BSD 2-Clause"},
							{Value: "mpl-2.0", DisplayName: "Mozilla Public License 2.0"},
							{Value: "unlicense", DisplayName: "The Unlicense"},
							{Value: "agpl-3.0", DisplayName: "GNU AGPLv3"},
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
	}, o.handleCreateRepositoryAction)
}

func (o *repositoryResourceType) handleCreateRepositoryAction(ctx context.Context, args *structpb.Struct) (*structpb.Struct, annotations.Annotations, error) {
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

	l.Info("github-connector: creating repository via action",
		zap.String("repo_name", name),
		zap.String("org_name", orgName),
	)

	// Build the Repository request
	newRepo := &github.Repository{
		Name: github.Ptr(name),
	}

	// Extract optional fields using SDK helpers
	if description, ok := actions.GetStringArg(args, "description"); ok && description != "" {
		newRepo.Description = github.Ptr(description)
	}

	if visibility, ok := actions.GetStringArg(args, "visibility"); ok && visibility != "" {
		if visibility == "public" || visibility == "private" {
			newRepo.Visibility = github.Ptr(visibility)
		} else {
			return nil, nil, fmt.Errorf("invalid visibility: %q (must be \"public\" or \"private\")", visibility)
		}
	}

	// add_readme maps to AutoInit in GitHub API
	if addReadme, ok := actions.GetBoolArg(args, "add_readme"); ok {
		newRepo.AutoInit = github.Ptr(addReadme)
	}

	if gitignoreTemplate, ok := actions.GetStringArg(args, "gitignore_template"); ok && gitignoreTemplate != "" {
		newRepo.GitignoreTemplate = github.Ptr(gitignoreTemplate)
	}

	if licenseTemplate, ok := actions.GetStringArg(args, "license_template"); ok && licenseTemplate != "" {
		newRepo.LicenseTemplate = github.Ptr(licenseTemplate)
	}

	// Create the repository via GitHub API
	createdRepo, resp, err := o.client.Repositories.Create(ctx, orgName, newRepo)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to create repository %s in org %s", name, orgName))
	}

	// Extract rate limit data for annotations
	var annos annotations.Annotations
	if rateLimitData, err := extractRateLimitData(resp); err == nil {
		annos.WithRateLimiting(rateLimitData)
	}

	l.Info("github-connector: repository created successfully via action",
		zap.String("repo_name", createdRepo.GetName()),
		zap.Int64("repo_id", createdRepo.GetID()),
		zap.String("repo_full_name", createdRepo.GetFullName()),
	)

	// Create the resource representation of the newly created repository
	repoResource, err := repositoryResource(ctx, createdRepo, parentResourceID)
	if err != nil {
		return nil, annos, fmt.Errorf("failed to create resource representation: %w", err)
	}

	// Generate entitlements for the newly created repository (reuse existing method)
	entitlements, _, _, err := o.Entitlements(ctx, repoResource, nil)
	if err != nil {
		return nil, annos, fmt.Errorf("failed to generate entitlements: %w", err)
	}

	// Fetch grants for the newly created repository by reusing the existing Grants method
	var grants []*v2.Grant
	pageToken := ""
	for {
		pToken := &pagination.Token{Token: pageToken}
		pageGrants, nextToken, _, err := o.Grants(ctx, repoResource, pToken)
		if err != nil {
			l.Warn("github-connector: failed to fetch grants for repository", zap.Error(err))
			break
		}
		grants = append(grants, pageGrants...)
		if nextToken == "" {
			break
		}
		pageToken = nextToken
	}

	// Build return values using SDK helpers
	resourceRv, err := actions.NewResourceReturnField("resource", repoResource)
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
