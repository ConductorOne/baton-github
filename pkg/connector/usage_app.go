package connector

import (
	"context"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
)

const (
	usageAppResourceID        = "github"
	usageAppDisplayName       = "GitHub Activity"
	usageAppAccessEntitlement = "access"
)

// resourceTypeUsageApp is a synthetic TRAIT_APP resource that usageEventFeed's
// UsageEvents target, since GitHub's real resource types carry no entitlement
// C1's usage uplift can key off of. Only synced when sync-last-activity is on.
var resourceTypeUsageApp = &v2.ResourceType{
	Id:          "usage-app",
	DisplayName: "GitHub Activity",
	Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_APP},
	Annotations: annotations.New(&v2.SkipGrants{}),
}

// usageAppBuilder syncs a single static App resource for usageEventFeed's
// UsageEvents to target.
type usageAppBuilder struct{}

func newUsageAppBuilder() *usageAppBuilder {
	return &usageAppBuilder{}
}

func (b *usageAppBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return resourceTypeUsageApp
}

func (b *usageAppBuilder) List(_ context.Context, _ *v2.ResourceId, _ resourceSdk.SyncOpAttrs) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	res, err := resourceSdk.NewAppResource(usageAppDisplayName, resourceTypeUsageApp, usageAppResourceID, nil)
	if err != nil {
		return nil, nil, err
	}
	return []*v2.Resource{res}, &resourceSdk.SyncOpResults{}, nil
}

func (b *usageAppBuilder) Entitlements(_ context.Context, resource *v2.Resource, _ resourceSdk.SyncOpAttrs) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	return []*v2.Entitlement{
		entitlement.NewAssignmentEntitlement(
			resource,
			usageAppAccessEntitlement,
			entitlement.WithGrantableTo(resourceTypeUser),
			entitlement.WithDisplayName("GitHub Access"),
			entitlement.WithDescription("Has access to GitHub"),
		),
	}, &resourceSdk.SyncOpResults{}, nil
}

func (b *usageAppBuilder) Grants(_ context.Context, _ *v2.Resource, _ resourceSdk.SyncOpAttrs) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	// Grants are intentionally not emitted: the usage uplift maps the login
	// actor directly to a synced app user, not via a grant.
	return nil, &resourceSdk.SyncOpResults{}, nil
}
