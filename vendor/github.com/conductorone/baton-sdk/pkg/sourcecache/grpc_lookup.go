package sourcecache

import (
	"context"
	"fmt"

	v1 "github.com/conductorone/baton-sdk/pb/c1/connectorapi/baton/v1"
)

// GRPCLookup is the connector-side Lookup implementation that talks to
// BatonSourceCacheService on the parent SDK.
//
// This is deliberately not routed through the session store: session data
// passes through the connector's local MemorySessionCache (otter), which
// would apply generic TTL/eviction policies to sync-scoped validator state.
// The dedicated service keeps the path uncached and the message shape
// explicit.
//
// The parent has exactly one active Lookup registered at a time (set per
// sync via SetSourceCache on the server), so the wire format carries no
// sync_id; routing is implicit.
type GRPCLookup struct {
	client v1.BatonSourceCacheServiceClient
}

// NewGRPCLookup returns a Lookup backed by the given client. A nil client
// yields NoopLookup so callers can configure the client optionally without
// nil checks at every call site.
func NewGRPCLookup(client v1.BatonSourceCacheServiceClient) Lookup {
	if client == nil {
		return NoopLookup{}
	}
	return &GRPCLookup{client: client}
}

func (g *GRPCLookup) LookupPreviousSourceCache(ctx context.Context, rowKind RowKind, scopeHash string) (Entry, bool, error) {
	if err := ValidateRowKind(rowKind); err != nil {
		return Entry{}, false, err
	}
	if err := ValidateScopeHash(scopeHash); err != nil {
		return Entry{}, false, err
	}
	resp, err := g.client.Lookup(ctx, v1.LookupRequest_builder{
		RowKind:   string(rowKind),
		ScopeHash: scopeHash,
	}.Build())
	if err != nil {
		return Entry{}, false, fmt.Errorf("source cache rpc lookup: %w", err)
	}
	if !resp.GetFound() {
		return Entry{}, false, nil
	}
	return Entry{ETag: resp.GetEtag()}, true, nil
}
