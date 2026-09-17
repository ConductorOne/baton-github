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

func TestPerKeySampledWarn_EachKeyGetsItsOwnBudget(t *testing.T) {
	// skippedOrgs is keyed per org so one noisy org's sampling budget can't
	// starve another org's first occurrence out of the log.
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := ctxzap.ToContext(context.Background(), zap.New(core))

	var p perKeySampledWarn
	orgs := []string{"org-a", "org-b", "org-c"}
	for i := 0; i < 9; i++ {
		org := orgs[i%len(orgs)]
		p.log(ctx, org, "org lacks audit-log access, skipping it for this pass", zap.String("org", org))
	}

	// Each org's first occurrence is its own occurrence 1, so all three log.
	require.Equal(t, 3, logs.Len())
	gotOrgs := make([]string, len(logs.All()))
	for i, entry := range logs.All() {
		gotOrgs[i] = entry.ContextMap()["org"].(string)
	}
	require.ElementsMatch(t, orgs, gotOrgs)
}

func TestPerKeySampledWarn_NoisyKeyDoesNotStarveNewKey(t *testing.T) {
	// Reproduces the diagnosability gap a shared counter has: org-a alone
	// drives the budget deep into a high sampling gap, then org-b's very
	// first failure must still surface immediately rather than landing on
	// org-a's non-sampled occurrence.
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := ctxzap.ToContext(context.Background(), zap.New(core))

	var p perKeySampledWarn
	for i := 0; i < 400; i++ {
		p.log(ctx, "org-a", "org lacks audit-log access, skipping it for this pass", zap.String("org", "org-a"))
	}
	logs.TakeAll() // discard org-a's own sampled lines; only org-b matters here

	p.log(ctx, "org-b", "org lacks audit-log access, skipping it for this pass", zap.String("org", "org-b"))

	require.Equal(t, 1, logs.Len(), "org-b's first failure must be logged even though org-a's counter is at 400")
	require.Equal(t, "org-b", logs.All()[0].ContextMap()["org"])
	require.Equal(t, uint64(1), logs.All()[0].ContextMap()["total_occurrences"])
}
