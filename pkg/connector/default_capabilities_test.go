package connector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The default builder's list is hand-maintained, and anything a real
// deployment can register but it omits is silently dropped from the published
// metadata -- which is the exact understatement the builder exists to fix.
// Two GitHub values are needed to cover everything: the enterprise role
// client provider decides between the read-only and the provisioning syncer,
// and its absence is also what keeps the license type registered.
func TestDefaultCapabilitiesCoverEverySyncerADeploymentCanRegister(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	advertised := make(map[string]bool)
	for _, syncer := range (&DefaultCapabilitiesBuilder{}).ResourceSyncers(ctx) {
		advertised[syncer.ResourceType(ctx).GetId()] = true
	}

	for _, gh := range []*GitHub{
		{syncSecrets: true, syncLastActivity: true, enterprises: []string{testEnterprise}},
		{syncSecrets: true, syncLastActivity: true, enterprises: []string{testEnterprise},
			newEnterpriseRoleClients: func(context.Context) (map[string]*githubEnterpriseAdministratorClient, error) {
				return nil, nil
			}},
	} {
		for _, syncer := range gh.ResourceSyncers(ctx) {
			id := syncer.ResourceType(ctx).GetId()
			require.True(t, advertised[id],
				"resource type %q is registered by a real deployment but missing from DefaultCapabilitiesBuilder", id)
		}
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
