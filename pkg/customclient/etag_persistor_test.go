package customclient

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/conductorone/baton-sdk/pkg/types/sessions"
	"github.com/stretchr/testify/require"
)

// fakeSessionStore is a minimal in-memory implementation of sessions.SessionStore
// suitable for unit tests. It honors WithPrefix by namespacing keys internally;
// no eviction, no size enforcement — those are properties of the production
// store, not our code under test.
type fakeSessionStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{data: make(map[string][]byte)}
}

func (f *fakeSessionStore) realKey(key string, opts []sessions.SessionStoreOption) string {
	bag := &sessions.SessionStoreBag{}
	for _, o := range opts {
		_ = o(context.Background(), bag)
	}
	if bag.Prefix != "" {
		return bag.Prefix + "/" + key
	}
	return key
}

func (f *fakeSessionStore) prefixFor(opts []sessions.SessionStoreOption) string {
	bag := &sessions.SessionStoreBag{}
	for _, o := range opts {
		_ = o(context.Background(), bag)
	}
	if bag.Prefix == "" {
		return ""
	}
	return bag.Prefix + "/"
}

func (f *fakeSessionStore) Get(_ context.Context, key string, opt ...sessions.SessionStoreOption) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[f.realKey(key, opt)]
	return v, ok, nil
}

func (f *fakeSessionStore) GetMany(_ context.Context, keys []string, opt ...sessions.SessionStoreOption) (map[string][]byte, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte)
	for _, k := range keys {
		if v, ok := f.data[f.realKey(k, opt)]; ok {
			out[k] = v
		}
	}
	return out, nil, nil
}

func (f *fakeSessionStore) Set(_ context.Context, key string, value []byte, opt ...sessions.SessionStoreOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[f.realKey(key, opt)] = append([]byte(nil), value...)
	return nil
}

func (f *fakeSessionStore) SetMany(_ context.Context, values map[string][]byte, opt ...sessions.SessionStoreOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range values {
		f.data[f.realKey(k, opt)] = append([]byte(nil), v...)
	}
	return nil
}

func (f *fakeSessionStore) Delete(_ context.Context, key string, opt ...sessions.SessionStoreOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, f.realKey(key, opt))
	return nil
}

func (f *fakeSessionStore) Clear(_ context.Context, opt ...sessions.SessionStoreOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := f.prefixFor(opt)
	if prefix == "" {
		f.data = make(map[string][]byte)
		return nil
	}
	for k := range f.data {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(f.data, k)
		}
	}
	return nil
}

func (f *fakeSessionStore) GetAll(_ context.Context, pageToken string, opt ...sessions.SessionStoreOption) (map[string][]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := f.prefixFor(opt)
	out := make(map[string][]byte)
	for k, v := range f.data {
		if prefix == "" {
			out[k] = v
			continue
		}
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out[k[len(prefix):]] = v
		}
	}
	_ = pageToken // single-page fake
	return out, "", nil
}

// size returns the number of entries currently held — testing helper.
func (f *fakeSessionStore) size() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.data)
}

func TestSessionPersistor_SaveAndLoadAll(t *testing.T) {
	ss := newFakeSessionStore()
	p := newSessionPersistor(ss, "scope-A")

	entry1 := &etagCacheEntry{
		ETag:        `"v1"`,
		Body:        []byte(`{"k":"v"}`),
		ContentType: "application/json",
		StoredAt:    time.Now(),
	}
	entry2 := &etagCacheEntry{
		ETag:     `"v2"`,
		Body:     []byte(`hello`),
		StoredAt: time.Now(),
	}
	require.NoError(t, p.Save(context.Background(), "k1", entry1))
	require.NoError(t, p.Save(context.Background(), "k2", entry2))

	loaded, err := p.LoadAll(context.Background())
	require.NoError(t, err)
	require.Len(t, loaded, 2)
	require.Equal(t, entry1.ETag, loaded["k1"].ETag)
	require.Equal(t, entry1.Body, loaded["k1"].Body)
	require.Equal(t, entry2.ETag, loaded["k2"].ETag)
}

func TestSessionPersistor_LoadAllFiltersStaleEntries(t *testing.T) {
	ss := newFakeSessionStore()
	p := newSessionPersistor(ss, "scope-A")

	now := time.Now()
	fresh := &etagCacheEntry{ETag: `"fresh"`, Body: []byte("ok"), StoredAt: now.Add(-1 * time.Hour)}
	stale := &etagCacheEntry{ETag: `"stale"`, Body: []byte("old"), StoredAt: now.Add(-8 * 24 * time.Hour)} // > 7d
	require.NoError(t, p.Save(context.Background(), "fresh", fresh))
	require.NoError(t, p.Save(context.Background(), "stale", stale))

	loaded, err := p.LoadAll(context.Background())
	require.NoError(t, err)
	require.Len(t, loaded, 1, "stale entry must be filtered out")
	_, hasFresh := loaded["fresh"]
	require.True(t, hasFresh)
	_, hasStale := loaded["stale"]
	require.False(t, hasStale)
}

func TestSessionPersistor_PrefixIsolation(t *testing.T) {
	// Two persistors with different scope hashes must not see each other's entries.
	ss := newFakeSessionStore()
	pA := newSessionPersistor(ss, "scope-A")
	pB := newSessionPersistor(ss, "scope-B")

	require.NoError(t, pA.Save(context.Background(), "shared-key", &etagCacheEntry{ETag: `"a"`, Body: []byte("a"), StoredAt: time.Now()}))
	require.NoError(t, pB.Save(context.Background(), "shared-key", &etagCacheEntry{ETag: `"b"`, Body: []byte("b"), StoredAt: time.Now()}))

	loadedA, err := pA.LoadAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, `"a"`, loadedA["shared-key"].ETag, "A must see its own entry, not B's")

	loadedB, err := pB.LoadAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, `"b"`, loadedB["shared-key"].ETag, "B must see its own entry, not A's")

	require.Equal(t, 2, ss.size(), "both entries stored under distinct prefixes")
}

func TestNoopPersistor(t *testing.T) {
	var p Persistor = noopPersistor{}
	loaded, err := p.LoadAll(context.Background())
	require.NoError(t, err)
	require.Empty(t, loaded)
	// Save must not panic or error.
	require.NoError(t, p.Save(context.Background(), "any", &etagCacheEntry{ETag: `"x"`}))
}
