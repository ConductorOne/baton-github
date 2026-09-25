package connector

import (
	"context"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
)

// DefaultCapabilitiesBuilder describes everything this connector can do, for
// the `capabilities` command alone.
//
// That command runs without credentials, so a connector built from real
// config would omit whatever its configuration did not switch on: enterprise
// roles and licenses need --enterprises, API keys need --sync-secrets, the
// usage app and its event feed need --sync-last-activity. The published
// metadata would then understate the connector for every tenant.
//
// It does not change what a running connector reports. C1 refreshes the
// capabilities it stores from the live connector on Validate and on every
// sync, so a deployment that cannot provision enterprise roles still
// advertises sync alone.
type DefaultCapabilitiesBuilder struct{}

// Metadata delegates to the real implementation so the account creation
// schema behind CAPABILITY_ACCOUNT_PROVISIONING cannot drift from it.
func (d *DefaultCapabilitiesBuilder) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
	return (&GitHub{}).Metadata(ctx)
}

func (d *DefaultCapabilitiesBuilder) Validate(_ context.Context) (annotations.Annotations, error) {
	return nil, nil
}

// ResourceSyncers lists every syncer, including the ones a given deployment
// may not register. The nil dependencies are never dereferenced: this builder
// only answers what methods each type implements.
func (d *DefaultCapabilitiesBuilder) ResourceSyncers(_ context.Context) []connectorbuilder.ResourceSyncerV2 {
	return []connectorbuilder.ResourceSyncerV2{
		OrgBuilder(nil, nil, nil, nil, false),
		TeamBuilder(nil, nil, false),
		UserBuilder(nil, nil, nil, nil, nil, nil),
		RepositoryBuilder(nil, nil, false, false),
		OrgRoleBuilder(nil, nil),
		InvitationBuilder(InvitationBuilderParams{}),
		AppBuilder(nil, nil),
		APITokenBuilder(nil, nil),
		newUsageAppBuilder(),
		// The provisioning-capable type: the published metadata describes what
		// the connector can do once configured for it.
		EnterpriseRoleProvisioningBuilder(nil, nil, nil, nil, nil),
		LicenseBuilder(nil, nil),
	}
}

// EventFeeds is what CAPABILITY_EVENT_FEED_V2 is derived from, so omitting it
// would drop the capability from the published metadata the same way a
// missing syncer drops a resource type.
func (d *DefaultCapabilitiesBuilder) EventFeeds(_ context.Context) []connectorbuilder.EventFeed {
	return []connectorbuilder.EventFeed{
		newUsageEventFeed(nil, nil),
	}
}
