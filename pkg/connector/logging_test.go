package connector

import (
	"context"
	"testing"

	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestShouldLogSample(t *testing.T) {
	tests := []struct {
		n    uint64
		want bool
	}{
		{0, true},
		{1, true},
		{2, false},
		{9, false},
		{10, true},
		{11, false},
		{99, false},
		{100, true},
		{101, false},
		{999, false},
		{1000, true},
		{1001, false},
		{1999, false},
		{2000, true},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, shouldLogSample(tt.n), "n=%d", tt.n)
	}
}

func TestSampledWarn_LogsOnlyOnSampledOccurrences(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := ctxzap.ToContext(context.Background(), zap.New(core))

	var s sampledWarn
	for i := 0; i < 12; i++ {
		s.log(ctx, "org lacks audit-log access, skipping it for this pass", zap.String("org", "octo-org"))
	}

	// Occurrences 1 and 10 should be logged; 2-9 and 11-12 should not.
	require.Equal(t, 2, logs.Len(), "expected exactly 2 sampled log lines out of 12 occurrences")

	first := logs.All()[0]
	require.Equal(t, uint64(1), first.ContextMap()["total_occurrences"])

	second := logs.All()[1]
	require.Equal(t, uint64(10), second.ContextMap()["total_occurrences"])
}

func TestSampledWarn_SharedAcrossDistinctOrgs(t *testing.T) {
	// skippedOrgs is intentionally a single shared counter on usageEventFeed,
	// not keyed per org: the sampling budget applies to the aggregate rate of
	// skip events across every org, not to each org individually.
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := ctxzap.ToContext(context.Background(), zap.New(core))

	var s sampledWarn
	orgs := []string{"org-a", "org-b", "org-c"}
	for i := 0; i < 9; i++ {
		s.log(ctx, "org lacks audit-log access, skipping it for this pass", zap.String("org", orgs[i%len(orgs)]))
	}

	// Only the very first occurrence (org-a) is logged; org-b's and org-c's
	// first occurrences do not each get their own log line under the shared
	// counter, since only occurrence 1 falls on the sampling schedule before 10.
	require.Equal(t, 1, logs.Len())
	require.Equal(t, "org-a", logs.All()[0].ContextMap()["org"])
}
