package connector

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/session"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/conductorone/baton-sdk/pkg/types/sessions"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
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
	directCollaboratorsOnly  bool
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
	return nil, nil, nil
}

func (o *repositoryResourceType) StaticEntitlements(_ context.Context, _ resourceSdk.SyncOpAttrs) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	rv := make([]*v2.Entitlement, 0, len(repoAccessLevels))
	for _, level := range repoAccessLevels {
		rv = append(rv, entitlement.NewPermissionEntitlement(nil, level,
			entitlement.WithDisplayName(fmt.Sprintf("Repo %s", titleCase(level))),
			entitlement.WithDescription(fmt.Sprintf("Access to repository in GitHub as %s", level)),
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

		// When direct-collaborators-only is enabled, org members whose only repo access
		// is via the org base permission won't appear in ListCollaborators. Add expandable
		// grants so the SDK resolves org membership into repo access.
		if o.directCollaboratorsOnly {
			basePerm, err := o.getOrgBasePermission(ctx, opts.Session, orgName, resource.ParentResourceId)
			if err != nil {
				l.Debug("failed to fetch org base permission, skipping org expansion", zap.Error(err))
			} else {
				orgResID := resource.ParentResourceId.Resource
				// Org admins always have admin on all repos
				adminEntitlementID := fmt.Sprintf("%s:%s:%s", resourceTypeOrg.Id, orgResID, orgRoleAdmin)
				for _, perm := range repoAccessLevels {
					rv = append(rv, grant.NewGrant(resource, perm, resource.ParentResourceId,
						grant.WithAnnotation(&v2.GrantExpandable{
							EntitlementIds:  []string{adminEntitlementID},
							Shallow:         true,
							ResourceTypeIds: []string{resourceTypeUser.Id},
						}),
					))
				}
				// Org members get access based on the org's default repo permission
				memberPerms := orgBasePermissionToRepoPermissions(basePerm)
				if len(memberPerms) > 0 {
					memberEntitlementID := fmt.Sprintf("%s:%s:%s", resourceTypeOrg.Id, orgResID, orgRoleMember)
					for _, perm := range memberPerms {
						rv = append(rv, grant.NewGrant(resource, perm, resource.ParentResourceId,
							grant.WithAnnotation(&v2.GrantExpandable{
								EntitlementIds:  []string{memberEntitlementID},
								Shallow:         true,
								ResourceTypeIds: []string{resourceTypeUser.Id},
							}),
						))
					}
				}
			}
		}

	case resourceTypeUser.Id:
		affiliation := "all"
		if o.directCollaboratorsOnly {
			affiliation = "direct"
		}
		listOpts := &github.ListCollaboratorsOptions{
			Affiliation: affiliation,
			ListOptions: github.ListOptions{
				Page:    page,
				PerPage: maxPageSize,
			},
		}
		users, resp, err := o.client.Repositories.ListCollaborators(ctx, orgName, resource.DisplayName, listOpts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusForbidden {
				l.Debug("insufficient access to list collaborators, skipping",
					zap.String("org", orgName),
					zap.String("repository", resource.DisplayName),
					zap.String("github_error", gitHubErrorMessage(err)),
				)
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
		orgID, err := parseResourceToGitHub(resource.ParentResourceId)
		if err != nil {
			return nil, nil, err
		}

		listOpts := &github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		}
		teams, resp, err := o.client.Repositories.ListTeams(ctx, orgName, resource.DisplayName, listOpts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusForbidden {
				l.Debug("insufficient access to list teams for repository, skipping",
					zap.String("org", orgName),
					zap.String("repository", resource.DisplayName),
					zap.String("github_error", gitHubErrorMessage(err)),
				)
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

				tr, err := teamResource(team, orgID, resource.ParentResourceId)
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
		team, resp, err := o.client.Teams.GetTeamByID(ctx, org.GetID(), principalID) //nolint:staticcheck,nolintlint // TODO: migrate to GetTeamBySlug
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
		team, resp, err := o.client.Teams.GetTeamByID(ctx, org.GetID(), principalID) //nolint:staticcheck,nolintlint // TODO: migrate to GetTeamBySlug
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

// orgBasePermissionSessionKey returns the session key for caching the org's default repo permission.
func orgBasePermissionSessionKey(orgID string) string {
	return "org_base_perm:" + orgID
}

// getOrgBasePermission fetches the org's default_repository_permission, caching in the session.
// Returns "read", "write", "admin", or "none".
func (o *repositoryResourceType) getOrgBasePermission(ctx context.Context, ss sessions.SessionStore, orgName string, orgResourceID *v2.ResourceId) (string, error) {
	key := orgBasePermissionSessionKey(orgResourceID.Resource)
	cached, found, err := session.GetJSON[string](ctx, ss, key)
	if err != nil {
		return "", fmt.Errorf("baton-github: error reading org base permission from session: %w", err)
	}
	if found {
		return cached, nil
	}

	org, resp, err := o.client.Organizations.Get(ctx, orgName)
	if err != nil {
		return "", wrapGitHubError(err, resp, "baton-github: failed to get organization")
	}

	perm := org.GetDefaultRepoPermission()
	if perm == "" {
		perm = "read" // GitHub default
	}

	if err := session.SetJSON(ctx, ss, key, perm); err != nil {
		return "", fmt.Errorf("baton-github: error caching org base permission: %w", err)
	}
	return perm, nil
}

// orgBasePermissionToRepoPermissions maps the org's default_repository_permission to
// the cumulative repo permission levels it grants.
func orgBasePermissionToRepoPermissions(basePerm string) []string {
	switch basePerm {
	case "admin":
		return []string{repoPermissionPull, repoPermissionTriage, repoPermissionPush, repoPermissionMaintain, repoPermissionAdmin}
	case "write":
		return []string{repoPermissionPull, repoPermissionTriage, repoPermissionPush}
	case "read":
		return []string{repoPermissionPull}
	default:
		return nil
	}
}

func repositoryBuilder(client *github.Client, orgCache *orgNameCache, omitArchivedRepositories bool, directCollaboratorsOnly bool) *repositoryResourceType {
	return &repositoryResourceType{
		resourceType:             resourceTypeRepository,
		client:                   client,
		orgCache:                 orgCache,
		omitArchivedRepositories: omitArchivedRepositories,
		directCollaboratorsOnly:  directCollaboratorsOnly,
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
