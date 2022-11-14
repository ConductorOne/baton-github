package connector

import (
	"context"
	"fmt"

	v2 "github.com/ductone/connector-sdk/pb/c1/connector/v2"
	"github.com/ductone/connector-sdk/pkg/annotations"
	"github.com/ductone/connector-sdk/pkg/pagination"
	"github.com/google/go-github/v41/github"
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
func repositoryResource(ctx context.Context, orgName string, repo *github.Repository, parentResourceID *v2.ResourceId) (*v2.Resource, error) {
	var annos annotations.Annotations
	annos.Append(&v2.ExternalLink{
		Url: repo.GetHTMLURL(),
	})
	annos.Append(&v2.V1Identifier{
		Id: fmt.Sprintf("repo:%d", repo.GetID()),
	})

	return &v2.Resource{
		Id:               fmtResourceId(resourceTypeRepository.Id, orgName, repo.GetID()),
		DisplayName:      repo.GetName(),
		ParentResourceId: parentResourceID,
		Annotations:      annos,
	}, nil
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

	orgName := getOrgName(parentID)

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
		rr, err := repositoryResource(ctx, orgName, repo, parentID)
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
		var annos annotations.Annotations
		annos.Append(&v2.V1Identifier{
			Id: fmt.Sprintf("repo:%s:role:%s", resource.Id, level),
		})
		rv = append(rv, &v2.Entitlement{
			Id:          fmtResourceRole(resource.Id, level),
			Resource:    resource,
			DisplayName: fmt.Sprintf("%s Repo %s", resource.DisplayName, titleCaser.String(level)),
			Description: fmt.Sprintf("Access to %s repository in Github", resource.DisplayName),
			Annotations: annos,
			GrantableTo: []*v2.ResourceType{resourceTypeUser, resourceTypeTeam},
			Purpose:     v2.Entitlement_PURPOSE_VALUE_PERMISSION,
			Slug:        level,
		})
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

	orgName := getOrgName(resource.Id)

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
				var annos annotations.Annotations

				if !hasPermission {
					continue
				}

				annos.Append(&v2.V1Identifier{
					Id: fmt.Sprintf("repo-grant:%s:%d:%s", resource.Id.Resource, user.GetID(), permission),
				})

				ur, err := userResource(ctx, user)
				if err != nil {
					return nil, "", nil, err
				}

				rv = append(rv, &v2.Grant{
					Entitlement: &v2.Entitlement{
						Id:       fmtResourceRole(resource.Id, permission),
						Resource: resource,
					},
					Id:          fmtResourceGrant(resource.Id, ur.Id, permission),
					Principal:   ur,
					Annotations: annos,
				})
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
				var annos annotations.Annotations

				if !hasPermission {
					continue
				}

				annos.Append(&v2.V1Identifier{
					Id: fmt.Sprintf("repo-grant:%s:%d:%s", resource.Id.Resource, team.GetID(), permission),
				})

				tr, err := teamResource(ctx, orgName, team, resource.ParentResourceId)
				if err != nil {
					return nil, "", nil, err
				}

				rv = append(rv, &v2.Grant{
					Entitlement: &v2.Entitlement{
						Id:       fmtResourceRole(resource.Id, permission),
						Resource: resource,
					},
					Id:          fmtResourceGrant(resource.Id, tr.Id, permission),
					Principal:   tr,
					Annotations: annos,
				})
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

func repositoryBuilder(client *github.Client) *repositoryResourceType {
	return &repositoryResourceType{
		resourceType: resourceTypeRepository,
		client:       client,
	}
}
