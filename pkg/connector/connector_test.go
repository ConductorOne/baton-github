package connector

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

// stubRefresher returns a successively-numbered token on each call.
type stubRefresher struct {
	mu    sync.Mutex
	calls int32
}

func (s *stubRefresher) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := atomic.AddInt32(&s.calls, 1)
	return &oauth2.Token{
		AccessToken: "minted-" + itoa(n),
		Expiry:      time.Now().Add(time.Hour),
	}, nil
}

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestForceRefreshTokenSource_ReusesValidToken(t *testing.T) {
	ref := &stubRefresher{}
	src := newForceRefreshTokenSource(
		&oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(time.Hour)},
		ref,
		zap.NewNop(),
	)

	for range 3 {
		tok, err := src.Token()
		require.NoError(t, err)
		require.Equal(t, "initial", tok.AccessToken)
	}
	require.Equal(t, int32(0), atomic.LoadInt32(&ref.calls))
}

func TestForceRefreshTokenSource_RefreshesWhenExpired(t *testing.T) {
	ref := &stubRefresher{}
	src := newForceRefreshTokenSource(
		&oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(-time.Minute)},
		ref,
		zap.NewNop(),
	)

	tok, err := src.Token()
	require.NoError(t, err)
	require.Equal(t, "minted-1", tok.AccessToken)
	require.Equal(t, int32(1), atomic.LoadInt32(&ref.calls))
}

func TestForceRefreshTokenSource_ForceRefreshNextBypassesValidCache(t *testing.T) {
	ref := &stubRefresher{}
	src := newForceRefreshTokenSource(
		&oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(time.Hour)},
		ref,
		zap.NewNop(),
	)

	src.ForceRefreshNext()
	tok, err := src.Token()
	require.NoError(t, err)
	require.Equal(t, "minted-1", tok.AccessToken)
	require.Equal(t, int32(1), atomic.LoadInt32(&ref.calls))

	// Flag must clear after a forced refresh; subsequent calls reuse cache.
	tok, err = src.Token()
	require.NoError(t, err)
	require.Equal(t, "minted-1", tok.AccessToken)
	require.Equal(t, int32(1), atomic.LoadInt32(&ref.calls), "no second refresh expected")
}

func TestForceRefreshTokenSource_RefresherErrorPropagates(t *testing.T) {
	src := newForceRefreshTokenSource(
		&oauth2.Token{AccessToken: "initial", Expiry: time.Now().Add(-time.Minute)},
		oauth2.TokenSource(stubErrSource{}),
		zap.NewNop(),
	)
	_, err := src.Token()
	require.ErrorContains(t, err, "boom")
}

type stubErrSource struct{}

func (stubErrSource) Token() (*oauth2.Token, error) { return nil, errors.New("boom") }

// --- wrapSyncerForRefresh -----------------------------------------------------

func newTestSrc() *forceRefreshTokenSource {
	return newForceRefreshTokenSource(
		&oauth2.Token{AccessToken: "tok", Expiry: time.Now().Add(time.Hour)},
		&stubRefresher{},
		zap.NewNop(),
	)
}

// fakeBaseSyncer implements only ResourceSyncerV2.
type fakeBaseSyncer struct {
	listCalls int32
}

func (f *fakeBaseSyncer) ResourceType(context.Context) *v2.ResourceType {
	return &v2.ResourceType{Id: "fake"}
}
func (f *fakeBaseSyncer) List(_ context.Context, _ *v2.ResourceId, _ resourceSdk.SyncOpAttrs) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	atomic.AddInt32(&f.listCalls, 1)
	return nil, nil, nil
}
func (f *fakeBaseSyncer) Entitlements(_ context.Context, _ *v2.Resource, _ resourceSdk.SyncOpAttrs) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	return nil, nil, nil
}
func (f *fakeBaseSyncer) Grants(_ context.Context, _ *v2.Resource, _ resourceSdk.SyncOpAttrs) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	return nil, nil, nil
}

// fakeProvisioningSyncer adds Grant/Revoke (V1 ResourceProvisionerLimited).
type fakeProvisioningSyncer struct {
	fakeBaseSyncer
}

func (f *fakeProvisioningSyncer) Grant(context.Context, *v2.Resource, *v2.Entitlement) (annotations.Annotations, error) {
	return nil, nil
}
func (f *fakeProvisioningSyncer) Revoke(context.Context, *v2.Grant) (annotations.Annotations, error) {
	return nil, nil
}

// fakeProvisioningStaticEntSyncer adds StaticEntitlements on top.
type fakeProvisioningStaticEntSyncer struct {
	fakeProvisioningSyncer
}

func (f *fakeProvisioningStaticEntSyncer) StaticEntitlements(context.Context, resourceSdk.SyncOpAttrs) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	return nil, nil, nil
}

// fakeDeleterSyncer adds Delete.
type fakeDeleterSyncer struct {
	fakeBaseSyncer
}

func (f *fakeDeleterSyncer) Delete(context.Context, *v2.ResourceId) (annotations.Annotations, error) {
	return nil, nil
}

// fakeDeleterAccountMgrSyncer adds CreateAccount + capability details.
type fakeDeleterAccountMgrSyncer struct {
	fakeDeleterSyncer
}

func (f *fakeDeleterAccountMgrSyncer) CreateAccount(
	context.Context,
	*v2.AccountInfo,
	*v2.LocalCredentialOptions,
) (connectorbuilder.CreateAccountResponse, []*v2.PlaintextData, annotations.Annotations, error) {
	return nil, nil, nil, nil
}
func (f *fakeDeleterAccountMgrSyncer) CreateAccountCapabilityDetails(context.Context) (*v2.CredentialDetailsAccountProvisioning, annotations.Annotations, error) {
	return nil, nil, nil
}

func TestWrapSyncerForRefresh_PreservesCapabilities(t *testing.T) {
	src := newTestSrc()
	cases := []struct {
		name       string
		inner      connectorbuilder.ResourceSyncerV2
		wantProv   bool
		wantStatic bool
		wantDel    bool
		wantAcct   bool
	}{
		{name: "base", inner: &fakeBaseSyncer{}},
		{name: "provisioning_static_ent", inner: &fakeProvisioningStaticEntSyncer{}, wantProv: true, wantStatic: true},
		{name: "deleter", inner: &fakeDeleterSyncer{}, wantDel: true},
		{name: "deleter_account_mgr", inner: &fakeDeleterAccountMgrSyncer{}, wantDel: true, wantAcct: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := wrapSyncerForRefresh(tc.inner, src)
			_, isProv := wrapped.(connectorbuilder.ResourceProvisionerLimited)
			_, isStatic := wrapped.(connectorbuilder.StaticEntitlementSyncerV2)
			_, isDel := wrapped.(connectorbuilder.ResourceDeleterLimited)
			_, isAcct := wrapped.(connectorbuilder.AccountManagerLimited)
			require.Equal(t, tc.wantProv, isProv, "ResourceProvisionerLimited")
			require.Equal(t, tc.wantStatic, isStatic, "StaticEntitlementSyncerV2")
			require.Equal(t, tc.wantDel, isDel, "ResourceDeleterLimited")
			require.Equal(t, tc.wantAcct, isAcct, "AccountManagerLimited")
		})
	}
}

func TestWrapSyncerForRefresh_ListForcesRefresh(t *testing.T) {
	src := newTestSrc()
	inner := &fakeBaseSyncer{}
	wrapped := wrapSyncerForRefresh(inner, src)

	// Sanity: cached token initially.
	tok, _ := src.Token()
	require.Equal(t, "tok", tok.AccessToken)

	_, _, err := wrapped.List(context.Background(), nil, resourceSdk.SyncOpAttrs{})
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&inner.listCalls))

	// After List, the next Token() call should refresh because ForceRefreshNext fired.
	tok, _ = src.Token()
	require.NotEqual(t, "tok", tok.AccessToken, "expected a fresh token after List")
}
