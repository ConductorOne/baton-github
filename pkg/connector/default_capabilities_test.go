package connector

import (
	"context"
	"testing"

	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/stretchr/testify/require"
)

// The default builder's list is hand-maintained, and anything such a
// deployment can register but it omits is silently dropped from the published
// metadata -- which is the exact understatement the builder exists to fix.
// The enterprise types are the deliberate exception, pinned by the test below.
func TestDefaultCapabilitiesCoverEverySyncerANonEnterpriseDeploymentRegisters(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	advertised := make(map[string]connectorbuilder.ResourceSyncerV2)
	for _, syncer := range (&DefaultCapabilitiesBuilder{}).ResourceSyncers(ctx) {
		advertised[syncer.ResourceType(ctx).GetId()] = syncer
	}

	gh := &GitHub{syncSecrets: true, syncLastActivity: true}
	for _, syncer := range gh.ResourceSyncers(ctx) {
		id := syncer.ResourceType(ctx).GetId()
		defaultSyncer, ok := advertised[id]
		require.True(t, ok,
			"resource type %q is registered by a real deployment but missing from DefaultCapabilitiesBuilder", id)

		// The SDK derives CAPABILITY_PROVISION by type-asserting the
		// syncer, so a matching ID does not imply a matching capability.
		if _, deploymentProvisions := syncer.(connectorbuilder.ResourceProvisionerV2Limited); deploymentProvisions {
			_, defaultProvisions := defaultSyncer.(connectorbuilder.ResourceProvisionerV2Limited)
			require.True(t, defaultProvisions,
				"resource type %q provisions in a real deployment, so DefaultCapabilitiesBuilder must register a provisioning syncer for it", id)
		}
	}
}

// The published metadata describes an account without an enterprise, so the
// --enterprises types stay out of it.
func TestDefaultCapabilitiesLeaveOutEnterpriseTypes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, syncer := range (&DefaultCapabilitiesBuilder{}).ResourceSyncers(ctx) {
		id := syncer.ResourceType(ctx).GetId()
		require.NotEqual(t, resourceTypeEnterpriseRole.Id, id)
		require.NotEqual(t, resourceTypeLicense.Id, id)
	}
}

// CAPABILITY_EVENT_FEED_V2 is derived from the registered feeds, so a feed the
// default builder does not list disappears from the published metadata.
func TestDefaultCapabilitiesCoverEveryEventFeed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	advertised := make(map[string]bool)
	for _, feed := range (&DefaultCapabilitiesBuilder{}).EventFeeds(ctx) {
		advertised[feed.EventFeedMetadata(ctx).GetId()] = true
	}

	gh := &GitHub{syncLastActivity: true}
	for _, feed := range gh.EventFeeds(ctx) {
		id := feed.EventFeedMetadata(ctx).GetId()
		require.True(t, advertised[id],
			"event feed %q is registered by a real deployment but missing from DefaultCapabilitiesBuilder", id)
	}
}
