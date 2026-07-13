package connector

// Pins the connector side of the ask/answer lookup continuation (the
// Lambda topology) and the token-carried validator contract:
//
//   - phase 1: a deferring lookup makes Grants fail with
//     ErrLookupDeferred BEFORE any upstream HTTP call (lookup-before-
//     fetch), with the whole stored-chain walk collected into ONE batch
//     ask;
//   - phase 2: the same call served from answers proceeds normally;
//   - a cursor carrying a resolved validator (hit or miss) performs NO
//     lookup at all — zero bounces for spawned siblings and chained
//     pages.

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/conductorone/baton-sdk/pkg/logging"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/sourcecache"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
)

// panicLookup fails the test if any lookup reaches it: pages with
// token-carried validators must not look anything up.
type panicLookup struct{ t *testing.T }

func (p panicLookup) LookupPreviousSourceCache(context.Context, sourcecache.RowKind, string) (sourcecache.Entry, bool, error) {
	p.t.Fatal("lookup called for a page with a token-carried validator")
	return sourcecache.Entry{}, false, nil
}

func newLookupTestClient(t *testing.T, mock *mockGitHubOrg) *github.Client {
	t.Helper()
	server := httptest.NewServer(mock.handler())
	t.Cleanup(server.Close)
	ghClient := github.NewClient(server.Client())
	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	ghClient.BaseURL = baseURL
	return ghClient
}

func memberRoleToken(t *testing.T, cursor pagedListCursor) string {
	t.Helper()
	cursorTok, err := cursor.marshal()
	require.NoError(t, err)
	bag := &pagination.Bag{}
	bag.Push(pagination.PageState{ResourceTypeID: orgRoleMember, Token: cursorTok})
	tok, err := bag.Marshal()
	require.NoError(t, err)
	return tok
}

func TestOrgGrantsLookupContinuation(t *testing.T) {
	ctx, err := logging.Init(t.Context())
	require.NoError(t, err)

	mock := newMockGitHubOrg(t)
	for i := int64(1); i <= 150; i++ {
		mock.addMember(i, false) // 2 member pages
	}
	ghClient := newLookupTestClient(t, mock)

	builder := OrgBuilder(ghClient, nil, newOrgNameCache(ghClient), []string{mockOrgLogin}, false, "test-auth")
	orgRes, err := organizationResource(ctx, &github.Organization{
		ID:    github.Ptr(mockOrgID),
		Login: github.Ptr(mockOrgLogin),
	}, nil, false)
	require.NoError(t, err)

	// Phase 1: continuation lookup with no answers. The role page-1 call
	// must die at the batched walk with ErrLookupDeferred, before ANY
	// upstream request, and the whole walk must be one recorded batch.
	cont := sourcecache.NewContinuationLookup(nil)
	_, _, err = builder.Grants(ctx, orgRes, resourceSdk.SyncOpAttrs{
		PageToken:   pagination.Token{Token: memberRoleToken(t, pagedListCursor{Page: 1})},
		Session:     &noOpSessionStore{},
		SourceCache: cont,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, sourcecache.ErrLookupDeferred,
		"deferred lookups must propagate unswallowed (Lambda phase 1)")
	require.Zero(t, mock.snapshotCounts()["members-200"],
		"phase 1 must defer before any upstream member fetch")

	asked := cont.Asked()
	require.Len(t, asked, storedChainBatchSize, "the stored-chain walk collects into one batch ask")

	// Phase 2: same call, answers attached (all not-found → cold path).
	answers := make([]sourcecache.Answer, 0, len(asked))
	for _, q := range asked {
		answers = append(answers, sourcecache.Answer{Query: q, Found: false})
	}
	served := sourcecache.NewContinuationLookup(answers)
	grants, res, err := builder.Grants(ctx, orgRes, resourceSdk.SyncOpAttrs{
		PageToken:   pagination.Token{Token: memberRoleToken(t, pagedListCursor{Page: 1})},
		Session:     &noOpSessionStore{},
		SourceCache: served,
	})
	require.NoError(t, err, "phase 2 must serve the same call from answers")
	require.Len(t, grants, maxPageSize, "cold page 1 fetches fresh")
	require.NotEmpty(t, res.NextPageToken, "cold chain continues serially")
	c := mock.snapshotCounts()
	require.Equal(t, 1, c["members-200"])
	require.Empty(t, served.Asked(), "a fully answered batch must not re-ask")
}

func TestTokenCarriedValidatorsSkipLookups(t *testing.T) {
	ctx, err := logging.Init(t.Context())
	require.NoError(t, err)

	mock := newMockGitHubOrg(t)
	for i := int64(1); i <= 250; i++ {
		mock.addMember(i, false) // 3 member pages
	}
	ghClient := newLookupTestClient(t, mock)
	pageURL := orgMembersPageURL(mockOrgLogin, orgRoleMember, 2)

	// Learn page 2's live ETag and row count with a plain fetch (this is
	// what a previous sync's manifest would hold).
	warmup, err := listUsersPageConditional(ctx, ghClient, sourcecache.NoopLookup{}, "test-auth", "org-members",
		pageURL, pagedListCursor{Page: 2}, maxPageSize)
	require.NoError(t, err)
	require.Len(t, warmup.Users, maxPageSize)
	validator := encodeValidator(warmup.Resp.Header.Get("ETag"), len(warmup.Users))
	_ = mock.snapshotCounts()

	// A hit-resolved cursor revalidates conditionally (304 → replay) with
	// NO lookup: panicLookup proves the token is the only source.
	page, err := listUsersPageConditional(ctx, ghClient, panicLookup{t: t}, "test-auth", "org-members",
		pageURL, pagedListCursor{Page: 2, Horizon: 3, Resolution: cursorValidatorHit, Etag: validator}, maxPageSize)
	require.NoError(t, err)
	require.True(t, page.Replayed, "matching token-carried validator must 304-replay")
	require.Zero(t, page.NextPage, "full page's probe candidate (3) is sibling-owned and clamped")
	c := mock.snapshotCounts()
	require.Equal(t, 1, c["members-304"])
	require.Zero(t, c["members-200"])

	// A miss-resolved cursor fetches cold with NO lookup and no
	// conditional header (the strict mock rejects unissued If-None-Match).
	page, err = listUsersPageConditional(ctx, ghClient, panicLookup{t: t}, "test-auth", "org-members",
		orgMembersPageURL(mockOrgLogin, orgRoleMember, 3), pagedListCursor{Page: 3, Resolution: cursorValidatorMiss}, maxPageSize)
	require.NoError(t, err)
	require.False(t, page.Replayed)
	require.Len(t, page.Users, 50)
	c = mock.snapshotCounts()
	require.Equal(t, 1, c["members-200"])
	require.Zero(t, c["members-304"])
}
