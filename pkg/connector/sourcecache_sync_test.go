package connector

// End-to-end source-cache replay harness for GitHub conditional requests,
// ported from the baton-microsoft-entra POC.
//
// Runs the real connector against a strict mock GitHub org with faithful
// conditional-request semantics (per-page body-hash ETags, ascending
// user-id ordering, Link headers on 200 only, NO Link on 304, 304 for a
// matching If-None-Match), through the real SDK sync loop on the Pebble
// engine, chaining each sync's c1z as the next sync's replay source.
//
// The paramount assertion is equivalence: after every warm sync a control
// sync (no previous c1z) runs against the same org state and the two
// files must be identical at the v2 reader surface. Scenarios also assert
// request-count ceilings (200s vs 304s) per round.
//
// The mock is strict: any unexpected request shape, any If-None-Match
// value that was never issued for that URL, or any malformed pagination
// parameter fails the test.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/connectorclient"
	"github.com/conductorone/baton-sdk/pkg/dotc1z"
	"github.com/conductorone/baton-sdk/pkg/logging"
	"github.com/conductorone/baton-sdk/pkg/sourcecache"
	sdkSync "github.com/conductorone/baton-sdk/pkg/sync"
	"github.com/conductorone/baton-sdk/pkg/types"
	"github.com/conductorone/baton-sdk/pkg/types/sessions"
	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// --- mock GitHub org ---------------------------------------------------------

const (
	mockOrgID    = int64(999)
	mockOrgLogin = "testorg"
)

type mockMember struct {
	ID    int64
	Login string
	Admin bool
}

type mockGitHubOrg struct {
	mu sync.Mutex
	t  *testing.T

	members map[int64]*mockMember

	// etagSalt perturbs every computed ETag; bumping it models upstream
	// validator eviction (every conditional request answers 200).
	etagSalt int

	// issued tracks every ETag handed out per request URI. A presented
	// If-None-Match that was never issued for that URI is a connector
	// bug (fabricated validator) and fails the test.
	issued map[string]map[string]bool

	counts map[string]int
}

func newMockGitHubOrg(t *testing.T) *mockGitHubOrg {
	return &mockGitHubOrg{
		t:       t,
		members: map[int64]*mockMember{},
		issued:  map[string]map[string]bool{},
		counts:  map[string]int{},
	}
}

func (m *mockGitHubOrg) addMember(id int64, admin bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.members[id]; exists {
		m.t.Fatalf("addMember: %d already exists", id)
	}
	m.members[id] = &mockMember{ID: id, Login: fmt.Sprintf("user-%d", id), Admin: admin}
}

func (m *mockGitHubOrg) removeMember(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.members[id]; !exists {
		m.t.Fatalf("removeMember: %d does not exist", id)
	}
	delete(m.members, id)
}

func (m *mockGitHubOrg) setAdmin(id int64, admin bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, exists := m.members[id]
	if !exists {
		m.t.Fatalf("setAdmin: %d does not exist", id)
	}
	mem.Admin = admin
}

func (m *mockGitHubOrg) renameMember(id int64, login string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, exists := m.members[id]
	if !exists {
		m.t.Fatalf("renameMember: %d does not exist", id)
	}
	mem.Login = login
}

// evictEtags rotates the salt: every stored validator stops matching and
// all conditional requests answer 200, modeling upstream cache eviction.
func (m *mockGitHubOrg) evictEtags() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.etagSalt++
}

func (m *mockGitHubOrg) snapshotCounts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for k, v := range m.counts {
		out[k] = v
	}
	m.counts = map[string]int{}
	return out
}

// membersForRole returns the role-filtered listing in ascending user-id
// order — GitHub's member ordering, which is what makes appends land on
// the LAST page. role=member excludes admins, matching GitHub semantics.
func (m *mockGitHubOrg) membersForRole(role string) []*mockMember {
	var out []*mockMember
	for _, mem := range m.members {
		if (role == "admin") == mem.Admin {
			out = append(out, mem)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func writeRateLimitHeaders(w http.ResponseWriter) {
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "4999")
	w.Header().Set("X-RateLimit-Reset", "4102444800")
}

func mustWriteJSON(t *testing.T, w http.ResponseWriter, obj any) {
	t.Helper()
	data, err := json.Marshal(obj)
	require.NoError(t, err)
	_, _ = w.Write(data)
}

func (m *mockGitHubOrg) orgJSON() map[string]any {
	return map[string]any{
		"id":       mockOrgID,
		"login":    mockOrgLogin,
		"html_url": "https://github.example/" + mockOrgLogin,
	}
}

func memberJSON(mem *mockMember) map[string]any {
	// Shape mirrors the real member listing: no name/email (GitHub does
	// not include them there), so a login change is what perturbs bytes.
	return map[string]any{
		"id":         mem.ID,
		"login":      mem.Login,
		"avatar_url": fmt.Sprintf("https://avatars.example/%d", mem.ID),
		"html_url":   "https://github.example/" + mem.Login,
		"type":       "User",
	}
}

func (m *mockGitHubOrg) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()

		if r.Method != http.MethodGet {
			m.t.Errorf("mock github: unexpected method %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeRateLimitHeaders(w)

		switch {
		case r.URL.Path == "/user/orgs":
			m.counts["user-orgs"]++
			mustWriteJSON(m.t, w, []map[string]any{m.orgJSON()})

		case r.URL.Path == "/user/memberships/orgs/"+mockOrgLogin:
			m.counts["org-membership"]++
			mustWriteJSON(m.t, w, map[string]any{
				"role":         "admin",
				"state":        "active",
				"organization": m.orgJSON(),
			})

		case r.URL.Path == "/organizations/"+strconv.FormatInt(mockOrgID, 10):
			m.counts["org-by-id"]++
			mustWriteJSON(m.t, w, m.orgJSON())

		case r.URL.Path == "/orgs/"+mockOrgLogin+"/members":
			m.handleMembersLocked(w, r)

		default:
			m.t.Errorf("mock github: unexpected request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			mustWriteJSON(m.t, w, map[string]any{"message": "Not Found"})
		}
	}
}

func (m *mockGitHubOrg) handleMembersLocked(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	role := q.Get("role")
	if role != "member" && role != "admin" {
		m.t.Errorf("mock github: members listing missing/invalid role %q (%s)", role, r.URL.String())
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	if pp := q.Get("per_page"); pp != strconv.Itoa(maxPageSize) {
		m.t.Errorf("mock github: members listing per_page %q, want %d (%s)", pp, maxPageSize, r.URL.String())
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		m.t.Errorf("mock github: members listing bad page %q (%s)", q.Get("page"), r.URL.String())
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	list := m.membersForRole(role)
	start := (page - 1) * maxPageSize
	end := start + maxPageSize
	if start > len(list) {
		start = len(list)
	}
	if end > len(list) {
		end = len(list)
	}
	pageMembers := list[start:end]

	body := make([]map[string]any, 0, len(pageMembers))
	for _, mem := range pageMembers {
		body = append(body, memberJSON(mem))
	}
	bodyBytes, err := json.Marshal(body)
	require.NoError(m.t, err)

	uri := r.URL.RequestURI()
	sum := sha256.Sum256([]byte(fmt.Sprintf("salt=%d|%s|%s", m.etagSalt, uri, bodyBytes)))
	etag := fmt.Sprintf(`W/"%x"`, sum[:12])

	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if !m.issued[uri][inm] {
			m.t.Errorf("mock github: If-None-Match %q for %s was never issued for that URL", inm, uri)
		}
		if inm == etag {
			// Faithful pessimistic 304: no body, no Link header, no ETag
			// beyond what the connector already holds.
			m.counts["members-304"]++
			m.counts[fmt.Sprintf("members-304:%s:p%d", role, page)]++
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	if m.issued[uri] == nil {
		m.issued[uri] = map[string]bool{}
	}
	m.issued[uri][etag] = true

	w.Header().Set("ETag", etag)
	if end < len(list) {
		next := cloneQueryWithPage(q, page+1)
		w.Header().Set("Link",
			fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, next.Encode()))
	}
	m.counts["members-200"]++
	m.counts[fmt.Sprintf("members-200:%s:p%d", role, page)]++
	_, _ = w.Write(bodyBytes)
}

func cloneQueryWithPage(q url.Values, page int) url.Values {
	next := url.Values{}
	for k, vs := range q {
		for _, v := range vs {
			next.Add(k, v)
		}
	}
	next.Set("page", strconv.Itoa(page))
	return next
}

// --- in-memory session store ---------------------------------------------------

// memSessionStore is a minimal in-memory sessions.SessionStore for the
// harness (the production session store rides gRPC; the builder's default
// NoOp store errors on Get, which would fail orgCache lookups). Options
// (sync-id namespacing) are ignored; Clear wipes everything, matching the
// per-sync cleanup semantics closely enough for the harness.
type memSessionStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemSessionStore() *memSessionStore {
	return &memSessionStore{data: map[string][]byte{}}
}

func (s *memSessionStore) Get(ctx context.Context, key string, opt ...sessions.SessionStoreOption) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok, nil
}

func (s *memSessionStore) GetMany(ctx context.Context, keys []string, opt ...sessions.SessionStoreOption) (map[string][]byte, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]byte{}
	var missing []string
	for _, k := range keys {
		if v, ok := s.data[k]; ok {
			out[k] = v
		} else {
			missing = append(missing, k)
		}
	}
	return out, missing, nil
}

func (s *memSessionStore) Set(ctx context.Context, key string, value []byte, opt ...sessions.SessionStoreOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

func (s *memSessionStore) SetMany(ctx context.Context, values map[string][]byte, opt ...sessions.SessionStoreOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range values {
		s.data[k] = v
	}
	return nil
}

func (s *memSessionStore) Delete(ctx context.Context, key string, opt ...sessions.SessionStoreOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *memSessionStore) Clear(ctx context.Context, opt ...sessions.SessionStoreOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = map[string][]byte{}
	return nil
}

func (s *memSessionStore) GetAll(ctx context.Context, pageToken string, opt ...sessions.SessionStoreOption) (map[string][]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range s.data {
		out[k] = v
	}
	return out, "", nil
}

// --- harness -----------------------------------------------------------------

type ghSyncHarness struct {
	t      *testing.T
	ctx    context.Context
	mock   *mockGitHubOrg
	cc     types.ConnectorClient
	tmpDir string
	syncN  int
}

func newGHSyncHarness(ctx context.Context, t *testing.T, mock *mockGitHubOrg) *ghSyncHarness {
	t.Helper()

	server := httptest.NewServer(mock.handler())
	t.Cleanup(server.Close)

	ghClient := github.NewClient(server.Client())
	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	ghClient.BaseURL = baseURL

	gh := &GitHub{
		client:    ghClient,
		orgs:      []string{mockOrgLogin},
		orgCache:  newOrgNameCache(ghClient),
		authScope: "test-auth",
	}

	srv, err := connectorbuilder.NewConnector(ctx, gh,
		connectorbuilder.WithSessionStore(newMemSessionStore()))
	require.NoError(t, err)

	// Serve the connector over local gRPC and talk to it through the real
	// connector client, mirroring how the CLI runs syncs.
	gs := grpc.NewServer()
	v2.RegisterConnectorServiceServer(gs, srv)
	v2.RegisterGrantsServiceServer(gs, srv)
	v2.RegisterEntitlementsServiceServer(gs, srv)
	v2.RegisterResourcesServiceServer(gs, srv)
	v2.RegisterResourceTypesServiceServer(gs, srv)
	v2.RegisterAssetServiceServer(gs, srv)
	v2.RegisterEventServiceServer(gs, srv)
	v2.RegisterResourceGetterServiceServer(gs, srv)
	v2.RegisterTicketsServiceServer(gs, srv)
	v2.RegisterActionServiceServer(gs, srv)
	v2.RegisterGrantManagerServiceServer(gs, srv)
	v2.RegisterResourceManagerServiceServer(gs, srv)
	v2.RegisterResourceDeleterServiceServer(gs, srv)
	v2.RegisterAccountManagerServiceServer(gs, srv)
	v2.RegisterCredentialManagerServiceServer(gs, srv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	cc := connectorclient.NewConnectorClient(ctx, conn)

	// In-process lookup delivery: the syncer installs its per-sync lookup
	// on the client, which forwards it to the builder (the CLI wrapper
	// does this same wiring in internal/connector).
	setter, ok := cc.(interface {
		SetSourceCacheSetter(sourcecache.SetLookup)
	})
	require.True(t, ok, "connector client must accept a source-cache setter")
	lookupSink, ok := srv.(sourcecache.SetLookup)
	require.True(t, ok, "connectorbuilder server must implement sourcecache.SetLookup")
	setter.SetSourceCacheSetter(lookupSink)

	return &ghSyncHarness{t: t, ctx: ctx, mock: mock, cc: cc, tmpDir: t.TempDir()}
}

// runSync executes one org-targeted full sync into a fresh Pebble c1z,
// optionally replaying from prevPath. Returns the new file's path.
func (h *ghSyncHarness) runSync(name string, prevPath string) string {
	h.t.Helper()
	h.syncN++
	path := filepath.Join(h.tmpDir, fmt.Sprintf("%02d-%s.c1z", h.syncN, name))

	store, err := dotc1z.NewStore(h.ctx, path,
		dotc1z.WithEngine(dotc1z.EnginePebble),
		dotc1z.WithTmpDir(h.tmpDir),
	)
	require.NoError(h.t, err)

	opts := []sdkSync.SyncOpt{
		sdkSync.WithConnectorStore(store),
		sdkSync.WithTmpDir(h.tmpDir),
		sdkSync.WithSyncResourceTypes([]string{"org"}),
	}
	if prevPath != "" {
		opts = append(opts, sdkSync.WithPreviousSyncC1ZPath(prevPath))
	}

	syncer, err := sdkSync.NewSyncer(h.ctx, h.cc, opts...)
	require.NoError(h.t, err)
	require.NoError(h.t, syncer.Sync(h.ctx))
	require.NoError(h.t, syncer.Close(h.ctx))
	return path
}

// snapshot reads a finished c1z at the v2 reader surface and returns
// id → canonical JSON for resources, entitlements, and grants.
func (h *ghSyncHarness) snapshot(path string) map[string]string {
	h.t.Helper()
	store, err := dotc1z.NewStore(h.ctx, path,
		dotc1z.WithEngine(dotc1z.EnginePebble),
		dotc1z.WithReadOnly(true),
		dotc1z.WithTmpDir(h.tmpDir),
	)
	require.NoError(h.t, err)
	defer func() { _ = store.Close(h.ctx) }()

	latest, err := store.SyncMeta().LatestFullSync(h.ctx)
	require.NoError(h.t, err)
	require.NotNil(h.t, latest)
	require.NoError(h.t, store.SetCurrentSync(h.ctx, latest.ID))

	out := map[string]string{}
	put := func(prefix, id string, msg proto.Message) {
		jb, err := protojson.Marshal(msg)
		require.NoError(h.t, err)
		// protojson output spacing is deliberately unstable; re-marshal
		// through encoding/json for canonical (sorted-key) bytes.
		var v any
		require.NoError(h.t, json.Unmarshal(jb, &v))
		cb, err := json.Marshal(v)
		require.NoError(h.t, err)
		key := prefix + ":" + id
		require.NotContains(h.t, out, key, "duplicate id at reader surface")
		out[key] = string(cb)
	}

	pageToken := ""
	for {
		resp, err := store.ListResources(h.ctx, v2.ResourcesServiceListResourcesRequest_builder{
			ResourceTypeId: "org",
			PageToken:      pageToken,
		}.Build())
		require.NoError(h.t, err)
		for _, r := range resp.GetList() {
			put("resource", r.GetId().GetResourceType()+"/"+r.GetId().GetResource(), r)
		}
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}

	pageToken = ""
	for {
		resp, err := store.ListEntitlements(h.ctx, v2.EntitlementsServiceListEntitlementsRequest_builder{
			PageToken: pageToken,
		}.Build())
		require.NoError(h.t, err)
		for _, e := range resp.GetList() {
			put("entitlement", e.GetId(), e)
		}
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}

	pageToken = ""
	for {
		resp, err := store.ListGrants(h.ctx, v2.GrantsServiceListGrantsRequest_builder{
			PageToken: pageToken,
		}.Build())
		require.NoError(h.t, err)
		for _, g := range resp.GetList() {
			put("grant", g.GetId(), g)
		}
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}

	return out
}

// requireEquivalent is the release-blocker check: a warm (replayed) sync
// must be byte-identical to an uncached control sync at the reader surface.
func (h *ghSyncHarness) requireEquivalent(warmPath, controlPath string, scenario string) {
	h.t.Helper()
	warm := h.snapshot(warmPath)
	control := h.snapshot(controlPath)
	require.Equal(h.t, control, warm,
		"%s: warm sync diverged from uncached control sync — replay equivalence violated", scenario)
}

// grantCount tallies grant rows in a snapshot.
func grantCount(snap map[string]string) int {
	n := 0
	for k := range snap {
		if len(k) > 6 && k[:6] == "grant:" {
			n++
		}
	}
	return n
}

// --- the scenarios -----------------------------------------------------------

func TestGitHubSourceCacheReplayEndToEnd(t *testing.T) {
	ctx, err := logging.Init(t.Context())
	require.NoError(t, err)

	mock := newMockGitHubOrg(t)

	// Org: 240 regular members (3 member pages: 100/100/40) and 10 admins
	// (1 admin page). Admin grants double (admins get member+admin rows),
	// so the expected grant count is 240 + 2*10 = 260.
	for i := int64(1); i <= 240; i++ {
		mock.addMember(i, false)
	}
	for i := int64(1000); i < 1010; i++ {
		mock.addMember(i, true)
	}

	h := newGHSyncHarness(ctx, t, mock)

	// --- Sync 1: cold ---------------------------------------------------------
	sync1 := h.runSync("cold", "")
	c1 := mock.snapshotCounts()
	require.Equal(t, 4, c1["members-200"], "cold sync fetches 3 member pages + 1 admin page")
	require.Zero(t, c1["members-304"])
	snap1 := h.snapshot(sync1)
	require.Equal(t, 260, grantCount(snap1))

	// --- Sync 2: warm, no upstream change --------------------------------------
	// Every page revalidates with a 304; zero member-listing rate-limit
	// units consumed; rows replayed from sync 1's file.
	sync2 := h.runSync("warm-noop", sync1)
	c2 := mock.snapshotCounts()
	require.Zero(t, c2["members-200"], "no-op warm sync must not re-fetch any member page")
	require.Equal(t, 4, c2["members-304"], "every page revalidates via 304")
	control2 := h.runSync("control-noop", "")
	h.requireEquivalent(sync2, control2, "no-op replay")

	// --- Sync 3: append at the end (GitHub id ordering) -------------------------
	// A new member with a high id lands on the LAST member page; the
	// prefix pages still 304. This is the case that breaks a
	// page-1-validates-the-collection design.
	mock.addMember(500, false)
	_ = mock.snapshotCounts() // discard the control run's counts
	sync3 := h.runSync("warm-append", sync2)
	c3 := mock.snapshotCounts()
	require.Equal(t, 1, c3["members-200"], "only the tail page re-fetches")
	require.Equal(t, 1, c3["members-200:member:p3"])
	require.Equal(t, 3, c3["members-304"], "prefix member pages + admin page still replay")
	control3 := h.runSync("control-append", "")
	h.requireEquivalent(sync3, control3, "append at end")
	require.Equal(t, 261, grantCount(h.snapshot(sync3)))

	// --- Sync 4: append past a FULL tail page (the probe case) ------------------
	// Grow the member list to exactly 300 (3 full pages), sync, then add
	// one more member. Pages 1-3 are byte-identical (304), but page 3 was
	// FULL, so the connector must probe page 4 and find the new member.
	// A design that trusted "no Link header on 304 => chain ends" would
	// silently drop this member.
	for i := int64(501); i <= 559; i++ {
		mock.addMember(i, false) // 241 existing members + 59 = 300
	}
	sync4a := h.runSync("warm-fill", sync3)
	control4a := h.runSync("control-fill", "")
	h.requireEquivalent(sync4a, control4a, "fill to page boundary")
	_ = mock.snapshotCounts()

	mock.addMember(600, false) // members: 301 → page 4 exists with 1 row
	sync4b := h.runSync("warm-probe", sync4a)
	c4 := mock.snapshotCounts()
	require.Equal(t, 4, c4["members-304"], "member pages 1-3 and the admin page all replay")
	require.Equal(t, 1, c4["members-200"])
	require.Equal(t, 1, c4["members-200:member:p4"], "full tail page forces a probe of page 4")
	control4b := h.runSync("control-probe", "")
	h.requireEquivalent(sync4b, control4b, "append past full tail")
	require.Equal(t, 321, grantCount(h.snapshot(sync4b)), "301 member grants + 2*10 admin grants")

	// --- Sync 5: removal shifts pages left --------------------------------------
	// Removing the FIRST member shifts every member page's bytes left; the
	// member count drops to exactly 300, so the previous page-4 scope is
	// simply never visited and its row does not carry over (the member it
	// held now lives on page 3, fetched fresh).
	mock.removeMember(1)
	_ = mock.snapshotCounts()
	sync5 := h.runSync("warm-remove", sync4b)
	c5 := mock.snapshotCounts()
	require.Equal(t, 3, c5["members-200"], "all three member pages shift and re-fetch")
	require.Equal(t, 1, c5["members-304"], "admin page still replays")
	control5 := h.runSync("control-remove", "")
	h.requireEquivalent(sync5, control5, "removal")

	// --- Sync 6: promotion member -> admin ---------------------------------------
	mock.setAdmin(2, true)
	sync6 := h.runSync("warm-promote", sync5)
	control6 := h.runSync("control-promote", "")
	h.requireEquivalent(sync6, control6, "promotion")

	// --- Sync 7: upstream evicts every validator ---------------------------------
	// Every conditional request answers 200: the warm sync degrades to
	// exactly a cold sync (fail toward cold), and the NEXT round is warm
	// again off the new validators.
	// State: 299 regular members (3 pages: 100/100/99) + 11 admins (1 page).
	mock.evictEtags()
	_ = mock.snapshotCounts()
	sync7 := h.runSync("warm-evicted", sync6)
	c7 := mock.snapshotCounts()
	require.Zero(t, c7["members-304"], "evicted validators can never 304")
	require.Equal(t, 4, c7["members-200"], "eviction degrades to a full cold fetch")
	control7 := h.runSync("control-evicted", "")
	h.requireEquivalent(sync7, control7, "total validator eviction")

	// --- Sync 8: recovery after eviction -----------------------------------------
	_ = mock.snapshotCounts()
	sync8 := h.runSync("warm-recovered", sync7)
	c8 := mock.snapshotCounts()
	require.Zero(t, c8["members-200"], "post-eviction round is fully warm again")
	require.Equal(t, 4, c8["members-304"])
	control8 := h.runSync("control-recovered", "")
	h.requireEquivalent(sync8, control8, "recovery after eviction")

	// --- Sync 9: warm-from-warm chain sanity --------------------------------------
	// sync8 was itself almost entirely replayed; it must still be a valid
	// replay source (manifest entries and scope stamps re-persisted).
	_ = mock.snapshotCounts()
	sync9 := h.runSync("warm-chain", sync8)
	c9 := mock.snapshotCounts()
	require.Zero(t, c9["members-200"])
	require.Equal(t, 4, c9["members-304"])
	control9 := h.runSync("control-chain", "")
	h.requireEquivalent(sync9, control9, "replay-only sync as replay source")
}
