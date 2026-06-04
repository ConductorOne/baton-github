package connector

import (
	"context"
	"fmt"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
)

// appResource builds a GitHub App resource from one organization installation.
// The org installations endpoint is the only org-wide-enumerable window onto
// GitHub Apps: each installation row carries the app's identity (app_id,
// app_slug) plus the installed scope. The resource is keyed by installation ID
// (unique and stable per org) and carries TRAIT_APP + an APP_REGISTRATION NHI
// annotation so c1 classifies it as a non-human identity.
func appResource(ctx context.Context, installation *github.Installation, parentResourceID *v2.ResourceId) (*v2.Resource, error) {
	appSlug := installation.GetAppSlug()
	displayName := appSlug
	if displayName == "" {
		displayName = fmt.Sprintf("app-%d", installation.GetAppID())
	}

	profile := map[string]interface{}{
		"app_id":          installation.GetAppID(),
		"app_slug":        appSlug,
		"installation_id": installation.GetID(),
	}
	if account := installation.GetAccount(); account != nil {
		profile["account_login"] = account.GetLogin()
	}
	if installation.TargetType != nil {
		profile["target_type"] = installation.GetTargetType()
	}
	if installation.RepositorySelection != nil {
		profile["repository_selection"] = installation.GetRepositorySelection()
	}

	opts := []resourceSdk.ResourceOption{
		resourceSdk.WithParentResourceID(parentResourceID),
		resourceSdk.WithAppTrait(resourceSdk.WithAppProfile(profile)),
		resourceSdk.WithNHIType(v2.NonHumanIdentityTrait_NHI_TYPE_APP_REGISTRATION, "github.app"),
	}
	if installation.HTMLURL != nil {
		opts = append(opts, resourceSdk.WithAnnotation(&v2.ExternalLink{Url: installation.GetHTMLURL()}))
	}

	return resourceSdk.NewResource(
		displayName,
		resourceTypeApp,
		installation.GetID(),
		opts...,
	)
}

type appResourceType struct {
	resourceType *v2.ResourceType
	client       *github.Client
	orgCache     *orgNameCache
}

func (o *appResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *appResourceType) Entitlements(ctx context.Context, resource *v2.Resource, opts resourceSdk.SyncOpAttrs) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	// GitHub Apps are synced read-only as NHI app registrations; no entitlements.
	return nil, &resourceSdk.SyncOpResults{}, nil
}

func (o *appResourceType) Grants(ctx context.Context, resource *v2.Resource, opts resourceSdk.SyncOpAttrs) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	// GitHub Apps are synced read-only as NHI app registrations; no grants.
	return nil, &resourceSdk.SyncOpResults{}, nil
}

func (o *appResourceType) List(
	ctx context.Context,
	parentID *v2.ResourceId,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	if parentID == nil {
		return nil, &resourceSdk.SyncOpResults{}, nil
	}

	bag, page, err := parsePageToken(opts.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeApp.Id})
	if err != nil {
		return nil, nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, opts.Session, parentID)
	if err != nil {
		return nil, nil, err
	}

	installations, resp, err := o.client.Organizations.ListInstallations(ctx, orgName, &github.ListOptions{
		Page:    page,
		PerPage: opts.PageToken.Size,
	})
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list organization app installations")
	}

	pageToken, annos, err := nextPageToken(bag, resp)
	if err != nil {
		return nil, nil, err
	}

	var rv []*v2.Resource
	for _, installation := range installations.Installations {
		resource, err := appResource(ctx, installation, parentID)
		if err != nil {
			return nil, &resourceSdk.SyncOpResults{
				NextPageToken: pageToken,
				Annotations:   annos,
			}, err
		}
		rv = append(rv, resource)
	}

	return rv, &resourceSdk.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   annos,
	}, nil
}

func AppBuilder(client *github.Client, orgCache *orgNameCache) *appResourceType {
	return &appResourceType{
		resourceType: resourceTypeApp,
		client:       client,
		orgCache:     orgCache,
	}
}
