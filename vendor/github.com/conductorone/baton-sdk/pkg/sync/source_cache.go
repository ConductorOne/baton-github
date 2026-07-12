package sync //nolint:revive,nolintlint // we can't change the package name for backwards compatibility

import (
	"context"
	"fmt"
	stdsync "sync"

	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorstore"
	"github.com/conductorone/baton-sdk/pkg/dotc1z"
	"github.com/conductorone/baton-sdk/pkg/sourcecache"
)

// Source-cache replay, syncer side. See
// proto/c1/connector/v2/annotation_source_cache.proto for the contract.
//
// Setup degrades, replay fails loudly. Any setup problem (capability
// absent, store engine unsupported, no usable previous sync) installs the
// no-op lookup: the connector never sees a previous validator, never gets
// a conditional-request hit, and therefore never emits SourceCacheReplay
// — which is what makes it safe to treat a replay annotation arriving
// while degraded as a hard error (the connector already skipped row
// generation; there is nothing to fall back to).

// syncerSourceCache is the per-sync source-cache state resolved by
// configureSourceCache.
//
// Write side and read side enable independently: the FIRST sync of a chain
// has no previous sync but must still stamp rows and write manifest
// entries, or the second sync would have nothing to replay. enabled covers
// the write side (capability declared + current store supports it); prev
// is non-nil only when a usable previous sync exists (read side — lookup
// hits and replay).
type syncerSourceCache struct {
	enabled bool
	// current is the writable output store's source-cache capability.
	current dotc1z.SourceCacheStore
	// prev is the previous sync's lookup/replay source (read-only). Nil
	// when no usable previous sync exists; lookups then miss and replay
	// annotations are hard errors.
	prev dotc1z.SourceCacheStore
	// prevReader is the same store as prev, typed for ReplaySourceCache.
	prevReader connectorstore.Reader
}

// prevStoreLookup adapts the previous store's manifest to the
// connector-facing Lookup. Mid-sync read errors are logged once and
// treated as misses: at lookup time the connector can still fetch fresh,
// so degrading beats failing the sync.
type prevStoreLookup struct {
	prev    dotc1z.SourceCacheStore
	logOnce *stdsync.Once
}

var _ sourcecache.Lookup = prevStoreLookup{}

func (p prevStoreLookup) LookupPreviousSourceCache(ctx context.Context, kind sourcecache.RowKind, scopeHash string) (sourcecache.Entry, bool, error) {
	entry, found, err := p.prev.LookupSourceCacheEntry(ctx, kind, scopeHash)
	if err != nil {
		p.logOnce.Do(func() {
			ctxzap.Extract(ctx).Warn("source cache lookup failed; treating as miss", zap.Error(err))
		})
		return sourcecache.Entry{}, false, nil //nolint:nilerr // intentional: a failed lookup degrades to a miss (connector fetches fresh) rather than failing the connector call
	}
	return entry, found, nil
}

// configureSourceCache resolves per-sync source-cache state from the
// connector's Validate response and installs the connector-facing lookup.
// Called once per Sync, after Validate.
func (s *syncer) configureSourceCache(ctx context.Context, resp *v2.ConnectorServiceValidateResponse) error {
	l := ctxzap.Extract(ctx)
	s.sourceCache = syncerSourceCache{}

	setLookup, canSetLookup := s.connector.(sourcecache.SetLookup)
	degrade := func(reason string) error {
		if canSetLookup {
			setLookup.SetSourceCache(ctx, sourcecache.NoopLookup{})
		}
		if reason != "" {
			l.Info("source cache disabled", zap.String("reason", reason))
		}
		return nil
	}

	capability := &v2.SourceCacheCapability{}
	annos := annotations.Annotations(resp.GetAnnotations())
	ok, err := annos.Pick(capability)
	if err != nil {
		return fmt.Errorf("error parsing source cache capability annotation: %w", err)
	}
	if !ok || capability.GetMode() != v2.SourceCacheCapability_MODE_READ_WRITE {
		// The common case; stay quiet.
		return degrade("")
	}
	current, ok := s.store.(dotc1z.SourceCacheStore)
	if !ok {
		return degrade("storage engine does not support source cache")
	}

	// Write side enabled: rows produced under a scope get stamped and
	// manifest entries get written, so this sync is usable as the NEXT
	// sync's replay source even when this one has nothing to replay from.
	s.sourceCache = syncerSourceCache{enabled: true, current: current}

	// Read side: a usable previous sync makes lookups hit and replay legal.
	var readReason string
	if s.previousSyncReader == nil {
		readReason = "no previous-sync c1z configured"
	} else if prev, ok := s.previousSyncReader.(dotc1z.SourceCacheStore); !ok {
		readReason = "previous-sync store engine does not support source cache"
	} else {
		s.sourceCache.prev = prev
		s.sourceCache.prevReader = s.previousSyncReader
	}

	lookup := sourcecache.Lookup(sourcecache.NoopLookup{})
	if s.sourceCache.prev != nil {
		lookup = prevStoreLookup{prev: s.sourceCache.prev, logOnce: &stdsync.Once{}}
	}
	if canSetLookup {
		setLookup.SetSourceCache(ctx, lookup)
	} else {
		// The connector declared the capability but the client offers no
		// way to deliver lookups. Its own lookup stays no-op, so every
		// scope misses and no replay annotations can legally arrive.
		l.Warn("source cache capability declared but connector client cannot receive lookups")
	}
	l.Info("source cache enabled",
		zap.Bool("replay_available", s.sourceCache.prev != nil),
		zap.String("replay_unavailable_reason", readReason),
	)
	return nil
}

// clearSourceCacheLookup detaches the per-sync lookup so a late RPC from
// the connector cannot read a store the syncer no longer owns.
func (s *syncer) clearSourceCacheLookup(ctx context.Context) {
	if setLookup, ok := s.connector.(sourcecache.SetLookup); ok {
		setLookup.SetSourceCache(ctx, nil)
	}
}

// sourceCachePage carries one list response's source-cache instructions
// from beginSourceCachePage (before rows are written) to
// finishSourceCachePage (after rows are written).
type sourceCachePage struct {
	kind      sourcecache.RowKind
	scopeHash string
	etag      string
	// replayed reports that beginSourceCachePage copied the previous
	// sync's rows for this scope into the current sync BEFORE the page's
	// own rows commit. Consumers that dedupe against "already synced this
	// sync" state (the resources path) must not skip this page's rows:
	// they are the overlay, and the already-present row is the stale
	// replayed base they exist to overwrite.
	replayed bool
	// deletedIDs are canonical-id tombstones (grant/entitlement ids,
	// resource BIDs); deletedPrincipalIDs are bare-object-id tombstones
	// applied scope-relatively. Both may arrive on any page of a scope
	// (replay annotation on the first page, scope annotation on every
	// page) and apply after the page's rows commit.
	deletedIDs          []string
	deletedPrincipalIDs []string
}

// beginSourceCachePage inspects a list response's annotations, performs
// any requested replay, and returns the context to write the page's rows
// under (stamped with the scope when one is present). A nil page means the
// response carried no source-cache instructions.
//
// rowCount is the number of rows in the response; a non-overlay replay
// that also returned rows gets a warning (the rows are upserted anyway).
func (s *syncer) beginSourceCachePage(
	ctx context.Context,
	kind sourcecache.RowKind,
	respAnnos annotations.Annotations,
	rowCount int,
) (context.Context, *sourceCachePage, error) {
	replay := &v2.SourceCacheReplay{}
	hasReplay, err := respAnnos.Pick(replay)
	if err != nil {
		return ctx, nil, fmt.Errorf("source cache: error parsing replay annotation: %w", err)
	}
	scope := &v2.SourceCacheScope{}
	hasScope, err := respAnnos.Pick(scope)
	if err != nil {
		return ctx, nil, fmt.Errorf("source cache: error parsing scope annotation: %w", err)
	}
	if !hasReplay && !hasScope {
		return ctx, nil, nil
	}

	if !s.sourceCache.enabled {
		if hasReplay {
			// The connector skipped row generation expecting a replay; with
			// source cache disabled there is nothing to replay from. This is
			// a connector bug (replay for a scope it never got a lookup hit
			// on), not a degradable condition.
			return ctx, nil, fmt.Errorf("source cache: connector requested replay for scope %q but source cache is disabled", replay.GetScopeHash())
		}
		// Scope annotations without the capability handshake are ignored.
		return ctx, nil, nil
	}

	page := &sourceCachePage{kind: kind}
	switch {
	case hasReplay && hasScope:
		if replay.GetScopeHash() != scope.GetScopeHash() {
			return ctx, nil, fmt.Errorf("source cache: replay scope %q and page scope %q disagree", replay.GetScopeHash(), scope.GetScopeHash())
		}
		page.scopeHash = replay.GetScopeHash()
	case hasReplay:
		page.scopeHash = replay.GetScopeHash()
	default:
		page.scopeHash = scope.GetScopeHash()
	}
	if err := sourcecache.ValidateScopeHash(page.scopeHash); err != nil {
		return ctx, nil, fmt.Errorf("source cache: %w", err)
	}
	// Prefer the scope annotation's etag (the freshest validator on
	// overlay pages); fall back to the replay's.
	page.etag = scope.GetEtag()
	if page.etag == "" {
		page.etag = replay.GetEtag()
	}
	// Tombstones may ride either annotation — the replay annotation on a
	// round's first page, the scope annotation on every page (so a
	// multi-page delta round never buffers deletions).
	page.deletedIDs = append(replay.GetDeletedIds(), scope.GetDeletedIds()...)
	page.deletedPrincipalIDs = append(replay.GetDeletedPrincipalIds(), scope.GetDeletedPrincipalIds()...)

	if hasReplay {
		if s.sourceCache.prev == nil {
			// Same invariant violation as the disabled case: the connector
			// can only have gotten a lookup hit if a previous source exists.
			return ctx, nil, fmt.Errorf("source cache: connector requested replay for scope %q but no previous sync is available", replay.GetScopeHash())
		}
		page.replayed = true
		if !replay.GetOverlay() && rowCount > 0 {
			// The contract says a 304-style replay page is empty, but rows
			// arriving here are more data, not less — upsert them on top of
			// the replayed base (overlay semantics) rather than failing the
			// sync. Kept lenient while the model is proven against real
			// providers.
			ctxzap.Extract(ctx).Warn("source cache: non-overlay replay returned rows; treating them as an overlay",
				zap.String("scope_hash", page.scopeHash),
				zap.Int("rows", rowCount),
			)
		}
		// Advisory check: a well-behaved connector only replays a scope
		// whose validator came from this sync's lookup, so a missing
		// previous manifest entry is suspicious — but not by itself data
		// loss (the stamped rows may still exist, e.g. a partially carried
		// file). The hard error below is reserved for a replay that
		// produces nothing.
		_, entryFound, err := s.sourceCache.prev.LookupSourceCacheEntry(ctx, kind, page.scopeHash)
		if err != nil {
			return ctx, nil, fmt.Errorf("source cache: error reading previous manifest for scope %q: %w", page.scopeHash, err)
		}
		if !entryFound {
			ctxzap.Extract(ctx).Warn("source cache: replay requested for scope with no previous manifest entry",
				zap.String("scope_hash", page.scopeHash))
		}
		res, err := s.sourceCache.current.ReplaySourceCache(ctx, s.sourceCache.prevReader, kind, page.scopeHash)
		if err != nil {
			return ctx, nil, fmt.Errorf("source cache: replay for scope %q failed: %w", page.scopeHash, err)
		}
		if res.Rows == 0 && !entryFound {
			// The connector skipped row generation expecting a base that
			// does not exist anywhere in the previous file — this sync
			// would silently drop the scope's rows.
			return ctx, nil, fmt.Errorf("source cache: replay for scope %q found no previous rows and no manifest entry; the connector replayed a scope it never looked up", page.scopeHash)
		}
		// Replay bypasses the connector-response path that normally arms
		// grant expansion (seeing GrantExpandable on returned rows), so a
		// sync whose expandable pages all replay would silently skip the
		// expansion phase without this.
		if kind == sourcecache.RowKindGrants && res.NeedsExpansion && !s.dontExpandGrants {
			s.state.SetNeedsExpansion()
		}
		ctxzap.Extract(ctx).Debug("source cache replayed scope",
			zap.String("row_kind", string(kind)),
			zap.String("scope_hash", page.scopeHash),
			zap.Int64("rows", res.Rows),
			zap.Bool("needs_expansion", res.NeedsExpansion),
			zap.Int("deleted_ids", len(page.deletedIDs)),
			zap.Int("deleted_principal_ids", len(page.deletedPrincipalIDs)),
		)
	}

	return sourcecache.WithScope(ctx, page.scopeHash), page, nil
}

// finishSourceCachePage runs after the page's rows committed: applies
// delta tombstones and, when the page carried a validator, writes the
// current sync's manifest entry. The entry write is last so a failed page
// can never leave a phantom hit for the next sync.
func (s *syncer) finishSourceCachePage(ctx context.Context, page *sourceCachePage) error {
	if page == nil {
		return nil
	}
	if len(page.deletedIDs) > 0 {
		if page.kind == sourcecache.RowKindGrants {
			// Grant-id tombstones resolve within the scope's own rows so
			// connector-custom grant-id shapes (unreachable by the global
			// bounded delete) work, and the cost stays bounded by the
			// scope's size.
			deleted, err := s.sourceCache.current.DeleteSourceCacheGrantsByIDInScope(ctx, page.scopeHash, page.deletedIDs)
			if err != nil {
				return fmt.Errorf("source cache: error applying grant deletions for scope %q: %w", page.scopeHash, err)
			}
			ctxzap.Extract(ctx).Debug("source cache applied grant-id deletions",
				zap.String("scope_hash", page.scopeHash),
				zap.Int("tombstones", len(page.deletedIDs)),
				zap.Int64("rows_deleted", deleted),
			)
		} else if err := s.sourceCache.current.DeleteSourceCacheRows(ctx, page.kind, page.deletedIDs); err != nil {
			return fmt.Errorf("source cache: error applying deletions for scope %q: %w", page.scopeHash, err)
		}
	}
	if len(page.deletedPrincipalIDs) > 0 {
		deleted, err := s.sourceCache.current.DeleteSourceCacheRowsInScope(ctx, page.kind, page.scopeHash, page.deletedPrincipalIDs)
		if err != nil {
			return fmt.Errorf("source cache: error applying scoped deletions for scope %q: %w", page.scopeHash, err)
		}
		ctxzap.Extract(ctx).Debug("source cache applied scoped deletions",
			zap.String("row_kind", string(page.kind)),
			zap.String("scope_hash", page.scopeHash),
			zap.Int("tombstones", len(page.deletedPrincipalIDs)),
			zap.Int64("rows_deleted", deleted),
		)
	}
	if page.etag != "" {
		if err := s.sourceCache.current.PutSourceCacheEntry(ctx, page.kind, page.scopeHash, page.etag); err != nil {
			return fmt.Errorf("source cache: error writing manifest entry for scope %q: %w", page.scopeHash, err)
		}
	}
	return nil
}
