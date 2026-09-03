package connector

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/google/go-github/v69/github"
	"github.com/migueleliasweb/go-github-mock/src/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestUsageEventFromAuditEntry(t *testing.T) {
	ts := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		entry *github.AuditEntry
		ok    bool
	}{
		{
			name: "valid entry",
			entry: &github.AuditEntry{
				Actor:     github.Ptr("octocat"),
				ActorID:   github.Ptr(int64(123)),
				OrgID:     github.Ptr(int64(456)),
				Timestamp: &github.Timestamp{Time: ts},
			},
			ok: true,
		},
		{
			name: "missing actor id",
			entry: &github.AuditEntry{
				Actor:     github.Ptr("octocat"),
				OrgID:     github.Ptr(int64(456)),
				Timestamp: &github.Timestamp{Time: ts},
			},
			ok: false,
		},
		{
			name: "missing org id",
			entry: &github.AuditEntry{
				Actor:     github.Ptr("octocat"),
				ActorID:   github.Ptr(int64(123)),
				Timestamp: &github.Timestamp{Time: ts},
			},
			ok: false,
		},
		{
			name: "missing timestamp",
			entry: &github.AuditEntry{
				Actor:   github.Ptr("octocat"),
				ActorID: github.Ptr(int64(123)),
				OrgID:   github.Ptr(int64(456)),
			},
			ok: false,
		},
		{
			name: "bot actor by login suffix",
			entry: &github.AuditEntry{
				Actor:     github.Ptr("dependabot[bot]"),
				ActorID:   github.Ptr(int64(123)),
				OrgID:     github.Ptr(int64(456)),
				Timestamp: &github.Timestamp{Time: ts},
			},
			ok: false,
		},
		{
			name: "bot actor by actor_is_bot field",
			entry: &github.AuditEntry{
				Actor:            github.Ptr("some-app-installation"),
				ActorID:          github.Ptr(int64(123)),
				OrgID:            github.Ptr(int64(456)),
				Timestamp:        &github.Timestamp{Time: ts},
				AdditionalFields: map[string]interface{}{"actor_is_bot": true},
			},
			ok: false,
		},
		{
			name: "actor_is_bot false overrides a non-matching suffix check",
			entry: &github.AuditEntry{
				Actor:            github.Ptr("octocat"),
				ActorID:          github.Ptr(int64(123)),
				OrgID:            github.Ptr(int64(456)),
				Timestamp:        &github.Timestamp{Time: ts},
				AdditionalFields: map[string]interface{}{"actor_is_bot": false},
			},
			ok: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt, evtTs, ok := usageEventFromAuditEntry("octo-org", tt.entry)
			require.Equal(t, tt.ok, ok)
			if !tt.ok {
				return
			}
			require.Equal(t, ts, evtTs)
			require.Equal(t, "123", evt.GetUsageEvent().GetActorResource().GetId().GetResource())
			require.Equal(t, "456", evt.GetUsageEvent().GetTargetResource().GetId().GetResource())
			require.Equal(t, resourceTypeUser.Id, evt.GetUsageEvent().GetActorResource().GetId().GetResourceType())
			require.Equal(t, resourceTypeOrg.Id, evt.GetUsageEvent().GetTargetResource().GetId().GetResourceType())
		})
	}
}

func TestUsageEventFromAuditEntry_IdFallback(t *testing.T) {
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	t.Run("uses the real document id when present", func(t *testing.T) {
		entry := &github.AuditEntry{
			Actor:      github.Ptr("octocat"),
			ActorID:    github.Ptr(int64(123)),
			OrgID:      github.Ptr(int64(456)),
			Timestamp:  &github.Timestamp{Time: ts},
			DocumentID: github.Ptr("real-doc-id"),
			Action:     github.Ptr("repo.create"),
		}
		evt, _, ok := usageEventFromAuditEntry("octo-org", entry)
		require.True(t, ok)
		require.Equal(t, "real-doc-id", evt.GetId())
	})

	t.Run("synthesizes a stable id when the document id is missing", func(t *testing.T) {
		entry := &github.AuditEntry{
			Actor:     github.Ptr("octocat"),
			ActorID:   github.Ptr(int64(123)),
			OrgID:     github.Ptr(int64(456)),
			Timestamp: &github.Timestamp{Time: ts},
			Action:    github.Ptr("repo.create"),
		}
		evt, _, ok := usageEventFromAuditEntry("octo-org", entry)
		require.True(t, ok)
		require.Equal(t, fmt.Sprintf("456:123:%d:repo.create", ts.Unix()), evt.GetId())
		require.NotEmpty(t, evt.GetId())
	})
}

func TestUsageEventFeed_ListEvents_GracefulDegradation(t *testing.T) {
	ctx := context.Background()

	httpClient := mock.NewMockedHTTPClient(
		mock.WithRequestMatch(mock.GetUserOrgs, []*github.Organization{{Login: github.Ptr("octo-org")}}),
		mock.WithRequestMatchHandler(
			mock.GetOrgsAuditLogByOrg,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			}),
		),
	)

	f := newUsageEventFeed(github.NewClient(httpClient), nil)

	events, state, _, err := f.ListEvents(ctx, nil, nil)
	require.NoError(t, err)
	require.Empty(t, events)
	require.False(t, state.HasMore)
}

func TestUsageEventFeed_ListEvents_FiltersToSinceBoundary(t *testing.T) {
	ctx := context.Background()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer1 := since.Add(2 * time.Hour)
	newer2 := since.Add(1 * time.Hour)
	older := since.Add(-1 * time.Hour)

	// Entries in descending order, as requested (Order: "desc").
	entries := []*github.AuditEntry{
		{Actor: github.Ptr("alice"), ActorID: github.Ptr(int64(1)), OrgID: github.Ptr(int64(9)), Timestamp: &github.Timestamp{Time: newer1}},
		{Actor: github.Ptr("bob"), ActorID: github.Ptr(int64(2)), OrgID: github.Ptr(int64(9)), Timestamp: &github.Timestamp{Time: newer2}},
		{Actor: github.Ptr("carol"), ActorID: github.Ptr(int64(3)), OrgID: github.Ptr(int64(9)), Timestamp: &github.Timestamp{Time: older}},
	}

	httpClient := mock.NewMockedHTTPClient(
		mock.WithRequestMatch(mock.GetUserOrgs, []*github.Organization{{Login: github.Ptr("octo-org")}}),
		mock.WithRequestMatch(mock.GetOrgsAuditLogByOrg, entries),
	)

	f := newUsageEventFeed(github.NewClient(httpClient), nil)

	events, state, _, err := f.ListEvents(ctx, timestamppb.New(since), nil)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.False(t, state.HasMore)
}

func TestUsageEventFeed_ListEvents_ZeroEarliestEventFallsBackToDefaultLookback(t *testing.T) {
	ctx := context.Background()

	var gotPhrase string
	httpClient := mock.NewMockedHTTPClient(
		mock.WithRequestMatch(mock.GetUserOrgs, []*github.Organization{{Login: github.Ptr("octo-org")}}),
		mock.WithRequestMatchHandler(
			mock.GetOrgsAuditLogByOrg,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPhrase = r.URL.Query().Get("phrase")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("[]"))
			}),
		),
	)

	f := newUsageEventFeed(github.NewClient(httpClient), nil)

	// A zero timestamppb.Timestamp mirrors a degenerate caller-supplied
	// start-at, which must not be trusted as a real boundary.
	events, state, _, err := f.ListEvents(ctx, &timestamppb.Timestamp{}, nil)
	require.NoError(t, err)
	require.Empty(t, events)
	require.False(t, state.HasMore)
	require.NotContains(t, gotPhrase, "0001-01-01")
	require.Contains(t, gotPhrase, "created:>=")
}

func TestUsageEventFeed_ListEvents_ContinuesPastAnAllFilteredPage(t *testing.T) {
	ctx := context.Background()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := since.Add(1 * time.Hour)

	// Page 1 is entirely bot activity - every entry gets filtered out by
	// usageEventFromAuditEntry, so no event is ever appended on this page.
	page1 := []*github.AuditEntry{
		{Actor: github.Ptr("dependabot[bot]"), ActorID: github.Ptr(int64(1)), OrgID: github.Ptr(int64(9)), Timestamp: &github.Timestamp{Time: newer}},
	}
	page2 := []*github.AuditEntry{
		{Actor: github.Ptr("octocat"), ActorID: github.Ptr(int64(2)), OrgID: github.Ptr(int64(9)), Timestamp: &github.Timestamp{Time: newer}},
	}

	calls := 0
	httpClient := mock.NewMockedHTTPClient(
		mock.WithRequestMatch(mock.GetUserOrgs, []*github.Organization{{Login: github.Ptr("octo-org")}}),
		mock.WithRequestMatchHandler(
			mock.GetOrgsAuditLogByOrg,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					w.Header().Set("Link", `<https://api.github.com/orgs/octo-org/audit-log?page=cursor2>; rel="next"`)
					_, _ = w.Write(mock.MustMarshal(page1))
					return
				}
				_, _ = w.Write(mock.MustMarshal(page2))
			}),
		),
	)

	f := newUsageEventFeed(github.NewClient(httpClient), nil)

	events, state, _, err := f.ListEvents(ctx, timestamppb.New(since), nil)
	require.NoError(t, err)
	require.Equal(t, 2, calls, "an all-filtered page must not be mistaken for the since boundary")
	require.Len(t, events, 1)
	require.Equal(t, "2", events[0].GetUsageEvent().GetActorResource().GetId().GetResource())
	require.False(t, state.HasMore)
}

func TestUsageEventFeed_ListEvents_ReturnsTightestRateLimit(t *testing.T) {
	ctx := context.Background()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := since.Add(1 * time.Hour)

	page1 := []*github.AuditEntry{
		{Actor: github.Ptr("octocat"), ActorID: github.Ptr(int64(1)), OrgID: github.Ptr(int64(9)), Timestamp: &github.Timestamp{Time: newer}},
	}
	page2 := []*github.AuditEntry{
		{Actor: github.Ptr("alice"), ActorID: github.Ptr(int64(2)), OrgID: github.Ptr(int64(9)), Timestamp: &github.Timestamp{Time: newer}},
	}

	calls := 0
	httpClient := mock.NewMockedHTTPClient(
		mock.WithRequestMatch(mock.GetUserOrgs, []*github.Organization{{Login: github.Ptr("octo-org")}}),
		mock.WithRequestMatchHandler(
			mock.GetOrgsAuditLogByOrg,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Ratelimit-Limit", "1750")
				if calls == 1 {
					// First call reports plenty of budget left.
					w.Header().Set("X-Ratelimit-Remaining", "500")
					w.Header().Set("Link", `<https://api.github.com/orgs/octo-org/audit-log?page=cursor2>; rel="next"`)
					_, _ = w.Write(mock.MustMarshal(page1))
					return
				}
				// Second call is the tighter one - this is the value that
				// should win.
				w.Header().Set("X-Ratelimit-Remaining", "10")
				_, _ = w.Write(mock.MustMarshal(page2))
			}),
		),
	)

	f := newUsageEventFeed(github.NewClient(httpClient), nil)

	events, _, annos, err := f.ListEvents(ctx, timestamppb.New(since), nil)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Len(t, events, 2)

	var rl v2.RateLimitDescription
	found, err := annos.Pick(&rl)
	require.NoError(t, err)
	require.True(t, found, "expected a rate-limit annotation to be returned")
	require.Equal(t, int64(10), rl.GetRemaining())
	require.Equal(t, int64(1750), rl.GetLimit())
}

func TestUsageEventFeed_ListEvents_AdvancesAcrossMultipleOrgs(t *testing.T) {
	ctx := context.Background()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := since.Add(1 * time.Hour)

	entriesByOrg := map[string][]*github.AuditEntry{
		"octo-org-a": {
			{Actor: github.Ptr("alice"), ActorID: github.Ptr(int64(1)), OrgID: github.Ptr(int64(9)), Timestamp: &github.Timestamp{Time: newer}},
		},
		"octo-org-b": {
			{Actor: github.Ptr("bob"), ActorID: github.Ptr(int64(2)), OrgID: github.Ptr(int64(8)), Timestamp: &github.Timestamp{Time: newer}},
		},
	}

	var seenOrgs []string
	httpClient := mock.NewMockedHTTPClient(
		mock.WithRequestMatch(mock.GetUserOrgs, []*github.Organization{
			{Login: github.Ptr("octo-org-a")},
			{Login: github.Ptr("octo-org-b")},
		}),
		mock.WithRequestMatchHandler(
			mock.GetOrgsAuditLogByOrg,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				org := strings.Split(r.URL.Path, "/")[2]
				seenOrgs = append(seenOrgs, org)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(mock.MustMarshal(entriesByOrg[org]))
			}),
		),
	)

	f := newUsageEventFeed(github.NewClient(httpClient), nil)

	events, state, _, err := f.ListEvents(ctx, timestamppb.New(since), nil)
	require.NoError(t, err)
	require.False(t, state.HasMore)
	require.Equal(t, []string{"octo-org-a", "octo-org-b"}, seenOrgs, "should walk both orgs, in order, within one call")
	require.Len(t, events, 2)

	actorIDs := []string{
		events[0].GetUsageEvent().GetActorResource().GetId().GetResource(),
		events[1].GetUsageEvent().GetActorResource().GetId().GetResource(),
	}
	require.ElementsMatch(t, []string{"1", "2"}, actorIDs)
}

func TestUsageEventFeed_ListEvents_ResumesFromPersistedCursor(t *testing.T) {
	ctx := context.Background()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := since.Add(1 * time.Hour)

	// Simulate a previous call that already finished "octo-org-a" and was
	// mid-page through "octo-org-b" with its own audit-log cursor.
	resumeToken := &usageEventPageToken{
		Orgs:           []string{"octo-org-a", "octo-org-b"},
		OrgIndex:       1,
		AuditLogCursor: "existing-cursor",
		Since:          since.Format(time.RFC3339),
	}
	cursorStr, err := resumeToken.marshal()
	require.NoError(t, err)

	var gotOrg, gotPage string
	httpClient := mock.NewMockedHTTPClient(
		mock.WithRequestMatchHandler(
			mock.GetOrgsAuditLogByOrg,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotOrg = strings.Split(r.URL.Path, "/")[2]
				gotPage = r.URL.Query().Get("page")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(mock.MustMarshal([]*github.AuditEntry{
					{Actor: github.Ptr("bob"), ActorID: github.Ptr(int64(2)), OrgID: github.Ptr(int64(8)), Timestamp: &github.Timestamp{Time: newer}},
				}))
			}),
		),
	)

	f := newUsageEventFeed(github.NewClient(httpClient), nil)

	events, state, _, err := f.ListEvents(ctx, nil, &pagination.StreamToken{Cursor: cursorStr})
	require.NoError(t, err)
	require.Equal(t, "octo-org-b", gotOrg, "should resume at the persisted org, not restart from octo-org-a")
	require.Equal(t, "existing-cursor", gotPage, "should resume with the persisted audit-log cursor")
	require.Len(t, events, 1)
	require.False(t, state.HasMore)
}

func TestUsageEventFeed_ListEvents_NoOrgs(t *testing.T) {
	ctx := context.Background()

	httpClient := mock.NewMockedHTTPClient(
		mock.WithRequestMatch(mock.GetUserOrgs, []*github.Organization{}),
	)

	f := newUsageEventFeed(github.NewClient(httpClient), nil)

	events, state, _, err := f.ListEvents(ctx, nil, nil)
	require.NoError(t, err)
	require.Empty(t, events)
	require.False(t, state.HasMore)
}

func TestUsageEventFeed_ListEvents_NilClient(t *testing.T) {
	ctx := context.Background()

	f := newUsageEventFeed(nil, nil)

	events, state, _, err := f.ListEvents(ctx, nil, &pagination.StreamToken{})
	require.NoError(t, err)
	require.Empty(t, events)
	require.False(t, state.HasMore)
}
