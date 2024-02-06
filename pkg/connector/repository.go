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
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v41/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
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

// repositoryResource returns a new connector resource for a Github repository.
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
	resourceType *v2.ResourceType
	client       *github.Client
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

	orgName, err := getOrgName(ctx, o.client, parentID)
	if err != nil {
		return nil, "", nil, err
	}

	opts := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: pt.Size,
		},
	}

	repos, resp, err := o.client.Repositories.ListByOrg(ctx, orgName, opts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("github-connector: failed to list repositories: %w", err)
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
			entitlement.WithDescription(fmt.Sprintf("Access to %s repository in Github", resource.DisplayName)),
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
	bag, page, err := parsePageToken(pToken.Token, resource.Id)
	if err != nil {
		return nil, "", nil, err
	}

	orgName, err := getOrgName(ctx, o.client, resource.ParentResourceId)
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
			ListOptions: github.ListOptions{Page: page},
		}
		users, resp, err := o.client.Repositories.ListCollaborators(ctx, orgName, resource.DisplayName, opts)
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connector: failed to list repos: %w", err)
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

				ur, err := userResource(ctx, user, user.GetEmail())
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
			Page: page,
		}
		teams, resp, err := o.client.Repositories.ListTeams(ctx, orgName, resource.DisplayName, opts)
		if err != nil {
			return nil, "", nil, fmt.Errorf("github-connector: failed to list repos: %w", err)
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

				rv = append(rv, grant.NewGrant(resource, permission, tr.Id, grant.WithAnnotation(&v2.V1Identifier{
					Id: fmt.Sprintf("repo-grant:%s:%d:%s", resource.Id.Resource, team.GetID(), permission),
				})))
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

	repo, _, err := o.client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to get repository: %w", err)
	}

	org := repo.GetOrganization()

	principalID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	switch principal.Id.ResourceType {
	case resourceTypeUser.Id:
		user, _, err := o.client.Users.GetByID(ctx, principalID)
		if err != nil {
			return nil, fmt.Errorf("github-connectorv2: failed to get user: %w", err)
		}

		_, _, e := o.client.Repositories.AddCollaborator(ctx, repo.GetOwner().GetLogin(), repo.GetName(), user.GetLogin(), &github.RepositoryAddCollaboratorOptions{
			Permission: en.Slug,
		})
		if e != nil {
			return nil, fmt.Errorf("github-connectorv2: failed to add user to a team: %w", e)
		}
	case resourceTypeTeam.Id:
		team, _, err := o.client.Teams.GetTeamByID(ctx, org.GetID(), principalID)
		if err != nil {
			return nil, fmt.Errorf("github-connectorv2: failed to get team: %w", err)
		}

		_, err = o.client.Teams.AddTeamRepoBySlug(ctx, org.GetLogin(), team.GetSlug(), repo.GetOwner().GetLogin(), repo.GetName(), &github.TeamAddTeamRepoOptions{
			Permission: en.Slug,
		})
		if err != nil {
			return nil, fmt.Errorf("github-connectorv2: failed to add team to a repo: %w", err)
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

	repo, _, err := o.client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to get repository: %w", err)
	}

	org := repo.GetOrganization()

	principalID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	switch principal.Id.ResourceType {
	case resourceTypeUser.Id:
		user, _, err := o.client.Users.GetByID(ctx, principalID)
		if err != nil {
			return nil, fmt.Errorf("github-connectorv2: failed to get user: %w", err)
		}

		_, e := o.client.Repositories.RemoveCollaborator(ctx, repo.GetOwner().GetLogin(), repo.GetName(), user.GetLogin())
		if e != nil {
			return nil, fmt.Errorf("github-connectorv2: failed to remove user from repo: %w", e)
		}
	case resourceTypeTeam.Id:
		team, _, err := o.client.Teams.GetTeamByID(ctx, org.GetID(), principalID)
		if err != nil {
			return nil, fmt.Errorf("github-connectorv2: failed to get team: %w", err)
		}

		_, err = o.client.Teams.RemoveTeamRepoBySlug(ctx, org.GetLogin(), team.GetSlug(), repo.GetOwner().GetLogin(), repo.GetName())
		if err != nil {
			return nil, fmt.Errorf("github-connectorv2: failed to remove team from repo: %w", err)
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

func repositoryBuilder(client *github.Client) *repositoryResourceType {
	return &repositoryResourceType{
		resourceType: resourceTypeRepository,
		client:       client,
	}
}
