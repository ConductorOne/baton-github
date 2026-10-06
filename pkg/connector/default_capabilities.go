package connector

import (
	"context"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
)

// DefaultCapabilitiesBuilder is used only by the `capabilities` command, which
// runs without config. The published metadata describes a GitHub account
// without an enterprise, including the types --sync-secrets and
// --sync-last-activity enable; --enterprises types are reported by the live
// connector only.
type DefaultCapabilitiesBuilder struct{}

func (d *DefaultCapabilitiesBuilder) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
	return (&GitHub{}).Metadata(ctx)
}

func (d *DefaultCapabilitiesBuilder) Validate(_ context.Context) (annotations.Annotations, error) {
	return nil, nil
}

// ResourceSyncers lists the syncers a GitHub account without an enterprise
// can register, including the ones --sync-secrets and --sync-last-activity
// gate. Their nil dependencies are never used.
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
	}
}

func (d *DefaultCapabilitiesBuilder) EventFeeds(_ context.Context) []connectorbuilder.EventFeed {
	return []connectorbuilder.EventFeed{
		newUsageEventFeed(nil, nil),
	}
}
