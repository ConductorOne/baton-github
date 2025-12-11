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
	"github.com/conductorone/baton-sdk/pkg/types/resource"
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
	ret, err := resource.NewResource(
		repo.GetName(),
		resourceTypeRepository,
		repo.GetID(),
		resource.WithAnnotation(
			&v2.ExternalLink{Url: repo.GetHTMLURL()},
			&v2.V1Identifier{Id: fmt.Sprintf("repo:%d", repo.GetID())},
		),
		resource.WithParentResourceID(parentResourceID),
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

func (o *repositoryResourceType) List(ctx context.Context, parentID *v2.ResourceId, pt *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	if parentID == nil {
		return nil, "", nil, nil
	}

	bag, page, err := parsePageToken(pt.Token, &v2.ResourceId{ResourceType: resourceTypeRepository.Id})
	if err != nil {
		return nil, "", nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, parentID)
	if err != nil {
		return nil, "", nil, err
	}

	opts := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		},
	}

	repos, resp, err := o.client.Repositories.ListByOrg(ctx, orgName, opts)
	if err != nil {
		return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to list repositories")
	}

	nextPage, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, err
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, "", nil, err
	}

	rv := make([]*v2.Resource, 0, len(repos))
	for _, repo := range repos {
		if o.omitArchivedRepositories && repo.GetArchived() {
			continue
		}
		rr, err := repositoryResource(ctx, repo, parentID)
		if err != nil {
			return nil, "", nil, err
		}
		rv = append(rv, rr)
	}

	return rv, pageToken, reqAnnos, nil
}

func (o *repositoryResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
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

	return rv, "", nil, nil
}

func (o *repositoryResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	pToken *pagination.Token,
) ([]*v2.Grant, string, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)
	bag, page, err := parsePageToken(pToken.Token, resource.Id)
	if err != nil {
		return nil, "", nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, resource.ParentResourceId)
	if err != nil {
		return nil, "", nil, err
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
		opts := &github.ListCollaboratorsOptions{
			Affiliation: "all",
			ListOptions: github.ListOptions{
				Page:    page,
				PerPage: maxPageSize,
			},
		}
		users, resp, err := o.client.Repositories.ListCollaborators(ctx, orgName, resource.DisplayName, opts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusForbidden {
				l.Warn("insufficient access to list collaborators", zap.String("repository", resource.DisplayName))
				pageToken, err := skipGrantsForResourceType(bag)
				if err != nil {
					return nil, "", nil, err
				}
				return nil, pageToken, nil, nil
			}
			if isNotFoundError(resp) {
				return nil, "", nil, uhttp.WrapErrors(codes.NotFound, fmt.Sprintf("repo: %s not found", resource.DisplayName))
			}
			return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to list collaborators")
		}

		nextPage, respAnnos, err := parseResp(resp)
		if err != nil {
			return nil, "", nil, err
		}
		reqAnnos = respAnnos

		err = bag.Next(nextPage)
		if err != nil {
			return nil, "", nil, err
		}

		for _, user := range users {
			for permission, hasPermission := range user.Permissions {
				if !hasPermission {
					continue
				}

				ur, err := userResource(ctx, user, user.GetEmail(), nil)
				if err != nil {
					return nil, "", nil, err
				}

				grant := grant.NewGrant(resource, permission, ur.Id, grant.WithAnnotation(&v2.V1Identifier{
					Id: fmt.Sprintf("repo-grant:%s:%d:%s", resource.Id.Resource, user.GetID(), permission),
				}))
				grant.Principal = ur
				rv = append(rv, grant)
			}
		}

	case resourceTypeTeam.Id:
		opts := &github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		}
		teams, resp, err := o.client.Repositories.ListTeams(ctx, orgName, resource.DisplayName, opts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusForbidden {
				l.Warn("insufficient access to list teams", zap.String("repository", resource.DisplayName))
				pageToken, err := skipGrantsForResourceType(bag)
				if err != nil {
					return nil, "", nil, err
				}
				return nil, pageToken, nil, nil
			}

			if isNotFoundError(resp) {
				return nil, "", nil, uhttp.WrapErrors(codes.NotFound, fmt.Sprintf("repo: %s not found", resource.DisplayName))
			}

			return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to list repository teams")
		}

		nextPage, respAnnos, err := parseResp(resp)
		if err != nil {
			return nil, "", nil, err
		}
		reqAnnos = respAnnos

		err = bag.Next(nextPage)
		if err != nil {
			return nil, "", nil, err
		}

		for _, team := range teams {
			for permission, hasPermission := range team.Permissions {
				if !hasPermission {
					continue
				}

				tr, err := teamResource(team, resource.ParentResourceId)
				if err != nil {
					return nil, "", nil, err
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
		return nil, "", nil, fmt.Errorf("unexpected resource type while fetching grants for repo")
	}

	pageToken, err := bag.Marshal()
	if err != nil {
		return nil, "", nil, err
	}

	return rv, pageToken, reqAnnos, nil
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
	if err := o.registerUpdateRepositoryAction(ctx, registry); err != nil {
		return err
	}
	if err := o.registerDeleteRepositoryAction(ctx, registry); err != nil {
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
				DisplayName: "Repository Name",
				Description: "The name of the repository to create",
				Field:       &config.Field_StringField{},
				IsRequired:  true,
			},
			{
				Name:        "parent",
				DisplayName: "Parent Organization",
				Description: "The organization to create the repository in",
				Field:       &config.Field_ResourceIdField{},
				IsRequired:  true,
			},
			{
				Name:        "description",
				DisplayName: "Description",
				Description: "A description of the repository",
				Field:       &config.Field_StringField{},
			},
			{
				Name:        "private",
				DisplayName: "Private",
				Description: "Whether the repository should be private (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "visibility",
				DisplayName: "Visibility",
				Description: "The visibility level of the repository",
				Field: &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "public", DisplayName: "Public"},
							{Value: "private", DisplayName: "Private"},
							{Value: "internal", DisplayName: "Internal (Enterprise only)"},
						},
					},
				},
			},
			{
				Name:        "has_issues",
				DisplayName: "Has Issues",
				Description: "Enable issues for this repository (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "has_projects",
				DisplayName: "Has Projects",
				Description: "Enable projects for this repository (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "has_wiki",
				DisplayName: "Has Wiki",
				Description: "Enable wiki for this repository (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "has_discussions",
				DisplayName: "Has Discussions",
				Description: "Enable discussions for this repository (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "auto_init",
				DisplayName: "Auto Initialize",
				Description: "Create an initial commit with empty README (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "gitignore_template",
				DisplayName: "Gitignore Template",
				Description: "Gitignore template to apply",
				Field: &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "", DisplayName: "None"},
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
							{Value: "", DisplayName: "None"},
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
			{
				Name:        "allow_squash_merge",
				DisplayName: "Allow Squash Merge",
				Description: "Allow squash-merging pull requests (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "allow_merge_commit",
				DisplayName: "Allow Merge Commit",
				Description: "Allow merging pull requests with a merge commit (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "allow_rebase_merge",
				DisplayName: "Allow Rebase Merge",
				Description: "Allow rebase-merging pull requests (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "allow_auto_merge",
				DisplayName: "Allow Auto Merge",
				Description: "Allow auto-merge on pull requests (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "delete_branch_on_merge",
				DisplayName: "Delete Branch on Merge",
				Description: "Automatically delete head branches after pull requests are merged (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "is_template",
				DisplayName: "Is Template",
				Description: "Make this repository available as a template (true/false)",
				Field:       &config.Field_BoolField{},
			},
		},
		ReturnTypes: []*config.Field{
			{Name: "success", Field: &config.Field_BoolField{}},
			{Name: "resource", Field: &config.Field_ResourceField{}},
		},
	}, o.handleCreateRepositoryAction)
}

func (o *repositoryResourceType) registerDeleteRepositoryAction(ctx context.Context, registry actions.ActionRegistry) error {
	return registry.Register(ctx, &v2.BatonActionSchema{
		Name:        "delete",
		DisplayName: "Delete Repository",
		Description: "Delete a repository from a GitHub organization",
		ActionType:  []v2.ActionType{v2.ActionType_ACTION_TYPE_RESOURCE_DELETE},
		Arguments: []*config.Field{
			{
				Name:        "resource",
				DisplayName: "Repository Resource",
				Description: "The repository resource to delete",
				Field:       &config.Field_ResourceIdField{},
				IsRequired:  true,
			},
			{
				Name:        "parent",
				DisplayName: "Parent Organization",
				Description: "The organization the repository belongs to",
				Field:       &config.Field_ResourceIdField{},
				IsRequired:  true,
			},
		},
		ReturnTypes: []*config.Field{
			{Name: "success", Field: &config.Field_BoolField{}},
		},
	}, o.handleDeleteRepositoryAction)
}

func (o *repositoryResourceType) registerUpdateRepositoryAction(ctx context.Context, registry actions.ActionRegistry) error {
	return registry.Register(ctx, &v2.BatonActionSchema{
		Name:        "update",
		DisplayName: "Update Repository",
		Description: "Update an existing repository in a GitHub organization",
		ActionType:  []v2.ActionType{v2.ActionType_ACTION_TYPE_RESOURCE_MUTATE},
		Arguments: []*config.Field{
			{
				Name:        "resource",
				DisplayName: "Repository Resource",
				Description: "The repository resource to update",
				Field:       &config.Field_ResourceIdField{},
				IsRequired:  true,
			},
			{
				Name:        "parent",
				DisplayName: "Parent Organization",
				Description: "The organization the repository belongs to",
				Field:       &config.Field_ResourceIdField{},
				IsRequired:  true,
			},
			{
				Name:        "name",
				DisplayName: "Repository Name",
				Description: "The new name of the repository (leave empty to keep current)",
				Field:       &config.Field_StringField{},
			},
			{
				Name:        "description",
				DisplayName: "Description",
				Description: "A description of the repository",
				Field:       &config.Field_StringField{},
			},
			{
				Name:        "homepage",
				DisplayName: "Homepage",
				Description: "A URL with more information about the repository",
				Field:       &config.Field_StringField{},
			},
			{
				Name:        "private",
				DisplayName: "Private",
				Description: "Whether the repository should be private (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "visibility",
				DisplayName: "Visibility",
				Description: "The visibility level of the repository",
				Field: &config.Field_StringField{
					StringField: &config.StringField{
						Options: []*config.StringFieldOption{
							{Value: "public", DisplayName: "Public"},
							{Value: "private", DisplayName: "Private"},
							{Value: "internal", DisplayName: "Internal (Enterprise only)"},
						},
					},
				},
			},
			{
				Name:        "has_issues",
				DisplayName: "Has Issues",
				Description: "Enable issues for this repository (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "has_projects",
				DisplayName: "Has Projects",
				Description: "Enable projects for this repository (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "has_wiki",
				DisplayName: "Has Wiki",
				Description: "Enable wiki for this repository (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "has_discussions",
				DisplayName: "Has Discussions",
				Description: "Enable discussions for this repository (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "default_branch",
				DisplayName: "Default Branch",
				Description: "The default branch of the repository",
				Field:       &config.Field_StringField{},
			},
			{
				Name:        "allow_squash_merge",
				DisplayName: "Allow Squash Merge",
				Description: "Allow squash-merging pull requests (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "allow_merge_commit",
				DisplayName: "Allow Merge Commit",
				Description: "Allow merging pull requests with a merge commit (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "allow_rebase_merge",
				DisplayName: "Allow Rebase Merge",
				Description: "Allow rebase-merging pull requests (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "allow_auto_merge",
				DisplayName: "Allow Auto Merge",
				Description: "Allow auto-merge on pull requests (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "delete_branch_on_merge",
				DisplayName: "Delete Branch on Merge",
				Description: "Automatically delete head branches after pull requests are merged (true/false)",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "archived",
				DisplayName: "Archived",
				Description: "Archive the repository (true/false). Note: You cannot unarchive repositories through the API",
				Field:       &config.Field_BoolField{},
			},
			{
				Name:        "is_template",
				DisplayName: "Is Template",
				Description: "Make this repository available as a template (true/false)",
				Field:       &config.Field_BoolField{},
			},
		},
		ReturnTypes: []*config.Field{
			{Name: "success", Field: &config.Field_BoolField{}},
			{Name: "resource", Field: &config.Field_ResourceField{}},
		},
	}, o.handleUpdateRepositoryAction)
}

func (o *repositoryResourceType) handleCreateRepositoryAction(ctx context.Context, args *structpb.Struct) (*structpb.Struct, annotations.Annotations, error) {
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

	if private, ok := actions.GetBoolArg(args, "private"); ok {
		newRepo.Private = github.Ptr(private)
	}

	if visibility, ok := actions.GetStringArg(args, "visibility"); ok && visibility != "" {
		if visibility == "public" || visibility == "private" || visibility == "internal" {
			newRepo.Visibility = github.Ptr(visibility)
		} else {
			l.Warn("github-connector: invalid visibility value, using default",
				zap.String("provided_visibility", visibility),
			)
		}
	}

	if hasIssues, ok := actions.GetBoolArg(args, "has_issues"); ok {
		newRepo.HasIssues = github.Ptr(hasIssues)
	}

	if hasProjects, ok := actions.GetBoolArg(args, "has_projects"); ok {
		newRepo.HasProjects = github.Ptr(hasProjects)
	}

	if hasWiki, ok := actions.GetBoolArg(args, "has_wiki"); ok {
		newRepo.HasWiki = github.Ptr(hasWiki)
	}

	if hasDiscussions, ok := actions.GetBoolArg(args, "has_discussions"); ok {
		newRepo.HasDiscussions = github.Ptr(hasDiscussions)
	}

	if autoInit, ok := actions.GetBoolArg(args, "auto_init"); ok {
		newRepo.AutoInit = github.Ptr(autoInit)
	}

	if gitignoreTemplate, ok := actions.GetStringArg(args, "gitignore_template"); ok && gitignoreTemplate != "" {
		newRepo.GitignoreTemplate = github.Ptr(gitignoreTemplate)
	}

	if licenseTemplate, ok := actions.GetStringArg(args, "license_template"); ok && licenseTemplate != "" {
		newRepo.LicenseTemplate = github.Ptr(licenseTemplate)
	}

	if allowSquashMerge, ok := actions.GetBoolArg(args, "allow_squash_merge"); ok {
		newRepo.AllowSquashMerge = github.Ptr(allowSquashMerge)
	}

	if allowMergeCommit, ok := actions.GetBoolArg(args, "allow_merge_commit"); ok {
		newRepo.AllowMergeCommit = github.Ptr(allowMergeCommit)
	}

	if allowRebaseMerge, ok := actions.GetBoolArg(args, "allow_rebase_merge"); ok {
		newRepo.AllowRebaseMerge = github.Ptr(allowRebaseMerge)
	}

	if allowAutoMerge, ok := actions.GetBoolArg(args, "allow_auto_merge"); ok {
		newRepo.AllowAutoMerge = github.Ptr(allowAutoMerge)
	}

	if deleteBranchOnMerge, ok := actions.GetBoolArg(args, "delete_branch_on_merge"); ok {
		newRepo.DeleteBranchOnMerge = github.Ptr(deleteBranchOnMerge)
	}

	if isTemplate, ok := actions.GetBoolArg(args, "is_template"); ok {
		newRepo.IsTemplate = github.Ptr(isTemplate)
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

	// Build return values using SDK helpers
	resourceRv, err := actions.NewResourceReturnField("resource", repoResource)
	if err != nil {
		return nil, annos, err
	}

	return actions.NewReturnValues(true, resourceRv), annos, nil
}

func (o *repositoryResourceType) handleDeleteRepositoryAction(ctx context.Context, args *structpb.Struct) (*structpb.Struct, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	// Extract the repository resource ID using SDK helper
	resourceID, err := actions.RequireResourceIDArg(args, "resource")
	if err != nil {
		return nil, nil, err
	}

	// Extract the parent org resource ID using SDK helper
	parentResourceID, err := actions.RequireResourceIDArg(args, "parent")
	if err != nil {
		return nil, nil, err
	}

	// Parse the repo ID from the resource
	repoID, err := strconv.ParseInt(resourceID.Resource, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid repository ID %s: %w", resourceID.Resource, err)
	}

	// Get the organization name from the parent resource ID
	orgName, err := o.orgCache.GetOrgName(ctx, parentResourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get organization name: %w", err)
	}

	// First, get the repository to find its name (needed for deletion)
	repo, resp, err := o.client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to get repository %d", repoID))
	}

	repoName := repo.GetName()

	l.Info("github-connector: deleting repository via action",
		zap.Int64("repo_id", repoID),
		zap.String("repo_name", repoName),
		zap.String("org_name", orgName),
	)

	// Delete the repository via GitHub API
	resp, err = o.client.Repositories.Delete(ctx, orgName, repoName)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to delete repository %s in org %s", repoName, orgName))
	}

	var annos annotations.Annotations
	if rateLimitData, err := extractRateLimitData(resp); err == nil {
		annos.WithRateLimiting(rateLimitData)
	}

	l.Info("github-connector: repository deleted successfully via action",
		zap.Int64("repo_id", repoID),
		zap.String("repo_name", repoName),
		zap.String("org_name", orgName),
	)

	return actions.NewReturnValues(true), annos, nil
}

func (o *repositoryResourceType) handleUpdateRepositoryAction(ctx context.Context, args *structpb.Struct) (*structpb.Struct, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	// Extract the repository resource ID using SDK helper
	resourceID, err := actions.RequireResourceIDArg(args, "resource")
	if err != nil {
		return nil, nil, err
	}

	// Extract the parent org resource ID using SDK helper
	parentResourceID, err := actions.RequireResourceIDArg(args, "parent")
	if err != nil {
		return nil, nil, err
	}

	// Parse the repo ID from the resource
	repoID, err := strconv.ParseInt(resourceID.Resource, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid repository ID %s: %w", resourceID.Resource, err)
	}

	// Get the organization name from the parent resource ID
	orgName, err := o.orgCache.GetOrgName(ctx, parentResourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get organization name: %w", err)
	}

	// First, get the current repository to find its name
	repo, resp, err := o.client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to get repository %d", repoID))
	}

	currentRepoName := repo.GetName()

	l.Info("github-connector: updating repository via action",
		zap.Int64("repo_id", repoID),
		zap.String("repo_name", currentRepoName),
		zap.String("org_name", orgName),
	)

	// Build the Repository update request
	updateRepo := &github.Repository{}

	// Track if any updates were provided
	hasUpdates := false

	// Extract optional fields using SDK helpers
	if name, ok := actions.GetStringArg(args, "name"); ok && name != "" {
		updateRepo.Name = github.Ptr(name)
		hasUpdates = true
	}

	if description, ok := actions.GetStringArg(args, "description"); ok {
		updateRepo.Description = github.Ptr(description)
		hasUpdates = true
	}

	if homepage, ok := actions.GetStringArg(args, "homepage"); ok {
		updateRepo.Homepage = github.Ptr(homepage)
		hasUpdates = true
	}

	if private, ok := actions.GetBoolArg(args, "private"); ok {
		updateRepo.Private = github.Ptr(private)
		hasUpdates = true
	}

	if visibility, ok := actions.GetStringArg(args, "visibility"); ok && visibility != "" {
		if visibility == "public" || visibility == "private" || visibility == "internal" {
			updateRepo.Visibility = github.Ptr(visibility)
			hasUpdates = true
		} else {
			l.Warn("github-connector: invalid visibility value, ignoring",
				zap.String("provided_visibility", visibility),
			)
		}
	}

	if hasIssues, ok := actions.GetBoolArg(args, "has_issues"); ok {
		updateRepo.HasIssues = github.Ptr(hasIssues)
		hasUpdates = true
	}

	if hasProjects, ok := actions.GetBoolArg(args, "has_projects"); ok {
		updateRepo.HasProjects = github.Ptr(hasProjects)
		hasUpdates = true
	}

	if hasWiki, ok := actions.GetBoolArg(args, "has_wiki"); ok {
		updateRepo.HasWiki = github.Ptr(hasWiki)
		hasUpdates = true
	}

	if hasDiscussions, ok := actions.GetBoolArg(args, "has_discussions"); ok {
		updateRepo.HasDiscussions = github.Ptr(hasDiscussions)
		hasUpdates = true
	}

	if defaultBranch, ok := actions.GetStringArg(args, "default_branch"); ok && defaultBranch != "" {
		updateRepo.DefaultBranch = github.Ptr(defaultBranch)
		hasUpdates = true
	}

	if allowSquashMerge, ok := actions.GetBoolArg(args, "allow_squash_merge"); ok {
		updateRepo.AllowSquashMerge = github.Ptr(allowSquashMerge)
		hasUpdates = true
	}

	if allowMergeCommit, ok := actions.GetBoolArg(args, "allow_merge_commit"); ok {
		updateRepo.AllowMergeCommit = github.Ptr(allowMergeCommit)
		hasUpdates = true
	}

	if allowRebaseMerge, ok := actions.GetBoolArg(args, "allow_rebase_merge"); ok {
		updateRepo.AllowRebaseMerge = github.Ptr(allowRebaseMerge)
		hasUpdates = true
	}

	if allowAutoMerge, ok := actions.GetBoolArg(args, "allow_auto_merge"); ok {
		updateRepo.AllowAutoMerge = github.Ptr(allowAutoMerge)
		hasUpdates = true
	}

	if deleteBranchOnMerge, ok := actions.GetBoolArg(args, "delete_branch_on_merge"); ok {
		updateRepo.DeleteBranchOnMerge = github.Ptr(deleteBranchOnMerge)
		hasUpdates = true
	}

	if archived, ok := actions.GetBoolArg(args, "archived"); ok {
		updateRepo.Archived = github.Ptr(archived)
		hasUpdates = true
	}

	if isTemplate, ok := actions.GetBoolArg(args, "is_template"); ok {
		updateRepo.IsTemplate = github.Ptr(isTemplate)
		hasUpdates = true
	}

	if !hasUpdates {
		return nil, nil, fmt.Errorf("no update fields provided")
	}

	// Update the repository via GitHub API
	updatedRepo, resp, err := o.client.Repositories.Edit(ctx, orgName, currentRepoName, updateRepo)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, fmt.Sprintf("failed to update repository %s in org %s", currentRepoName, orgName))
	}

	// Extract rate limit data for annotations
	var annos annotations.Annotations
	if rateLimitData, err := extractRateLimitData(resp); err == nil {
		annos.WithRateLimiting(rateLimitData)
	}

	l.Info("github-connector: repository updated successfully via action",
		zap.Int64("repo_id", updatedRepo.GetID()),
		zap.String("repo_name", updatedRepo.GetName()),
		zap.String("repo_full_name", updatedRepo.GetFullName()),
	)

	// Create the resource representation of the updated repository
	repoResource, err := repositoryResource(ctx, updatedRepo, parentResourceID)
	if err != nil {
		return nil, annos, fmt.Errorf("failed to create resource representation: %w", err)
	}

	// Build return values using SDK helpers
	resourceRv, err := actions.NewResourceReturnField("resource", repoResource)
	if err != nil {
		return nil, annos, err
	}

	return actions.NewReturnValues(true, resourceRv), annos, nil
}
