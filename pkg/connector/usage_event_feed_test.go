package connector

import (
	"context"
	"net/http"
	"testing"
	"time"

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

	// A zero timestamppb.Timestamp mirrors what an unset/degenerate
	// caller-supplied start-at looks like after round-tripping through
	// timestamppb - it must not be trusted as a real boundary.
	events, state, _, err := f.ListEvents(ctx, &timestamppb.Timestamp{}, nil)
	require.NoError(t, err)
	require.Empty(t, events)
	require.False(t, state.HasMore)
	require.NotContains(t, gotPhrase, "0001-01-01")
	require.Contains(t, gotPhrase, "created:>=")
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
