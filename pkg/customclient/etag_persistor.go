package customclient

import (
	"context"
	"fmt"
	"time"

	"github.com/conductorone/baton-sdk/pkg/session"
	"github.com/conductorone/baton-sdk/pkg/types/sessions"
)

const (
	// persistedEntryTTL caps how stale a persisted entry can be before we
	// refuse to load it. Self-pruning without depending on session-store-side
	// eviction; protects against stale ETags after long outages and bounds
	// how much storage one installation can occupy.
	persistedEntryTTL = 7 * 24 * time.Hour

	// persistedMaxEntryBytes is the upper bound on a single persisted entry's
	// body. Larger bodies stay in the in-process Otter cache (still benefiting
	// warm reruns) but skip persistence so we don't blow through the session
	// store's grpc message size limit or chew through storage budgets.
	persistedMaxEntryBytes = 256 * 1024

	// sessionStoreBasePrefix scopes our cache entries within the session store
	// so we don't collide with other connector caches. The full prefix is
	// `<base>:<auth-scope-hash>`.
	sessionStoreBasePrefix = "github-etag-cache"
)

// Persistor saves and loads ETag cache entries across sync runs. The transport
// reads/writes synchronously to its in-memory Otter cache; persistence is
// best-effort and runs through this layer asynchronously, so Save errors must
// never propagate into the HTTP request path.
type Persistor interface {
	// LoadAll returns every entry that should be hydrated into the in-process
	// cache at sync start. Implementations are expected to filter out entries
	// stale beyond persistedEntryTTL.
	LoadAll(ctx context.Context) (map[string]*etagCacheEntry, error)
	// Save persists a single entry. Idempotent; safe to call repeatedly.
	Save(ctx context.Context, key string, entry *etagCacheEntry) error
}

// noopPersistor is the default when no session store is wired in. It preserves
// pre-1.5 behavior — in-process Otter cache only, no cross-run durability —
// so callers don't have to nil-check.
type noopPersistor struct{}

func (noopPersistor) LoadAll(_ context.Context) (map[string]*etagCacheEntry, error) {
	return nil, nil
}

func (noopPersistor) Save(_ context.Context, _ string, _ *etagCacheEntry) error {
	return nil
}

// sessionPersistor backs the cache with baton-sdk's session store. Entries are
// namespaced by a transport-scoped prefix so two installations sharing a
// process don't read each other's cache state.
type sessionPersistor struct {
	ss     sessions.SessionStore
	prefix string
}

func newSessionPersistor(ss sessions.SessionStore, scopeHash string) *sessionPersistor {
	return &sessionPersistor{
		ss:     ss,
		prefix: sessionStoreBasePrefix + ":" + scopeHash,
	}
}

// prefixOpt returns the prefix option to scope read/writes to this transport's
// namespace within the session store.
func (p *sessionPersistor) prefixOpt() sessions.SessionStoreOption {
	return sessions.WithPrefix(p.prefix)
}

// LoadAll pulls every entry under our prefix, drops entries older than
// persistedEntryTTL, and returns them keyed exactly as the transport keys
// them in memory.
func (p *sessionPersistor) LoadAll(ctx context.Context) (map[string]*etagCacheEntry, error) {
	raw, err := session.GetAllJSON[*etagCacheEntry](ctx, p.ss, p.prefixOpt())
	if err != nil {
		return nil, fmt.Errorf("etag persistor: load all: %w", err)
	}
	fresh := make(map[string]*etagCacheEntry, len(raw))
	cutoff := time.Now().Add(-persistedEntryTTL)
	for k, v := range raw {
		if v == nil || v.StoredAt.Before(cutoff) {
			continue
		}
		fresh[k] = v
	}
	return fresh, nil
}

// Save persists one entry. Returns an error only on serialization or store
// failure; the writer goroutine treats this as best-effort and logs without
// retrying.
func (p *sessionPersistor) Save(ctx context.Context, key string, entry *etagCacheEntry) error {
	if entry == nil {
		return nil
	}
	if err := session.SetJSON(ctx, p.ss, key, entry, p.prefixOpt()); err != nil {
		return fmt.Errorf("etag persistor: save %q: %w", key, err)
	}
	return nil
}
