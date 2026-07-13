package connector

// Randomized churn equivalence fuzzer for the GitHub source-cache warm
// path, ported from the baton-microsoft-entra POC.
//
// The scripted scenarios in sourcecache_sync_test.go each pin one known
// hazard. This test covers the space BETWEEN them: every round applies a
// random batch of org mutations (member add/remove anywhere in the id
// order, promote/demote, login renames, occasional total ETag eviction),
// runs a warm sync chained off the previous round's output, runs a fresh
// uncached control sync of the same org state, and requires the two to be
// byte-identical at the reader surface. Any divergence — a stale replay,
// a dropped tail page, a missed boundary shift — fails with the round's
// seed and mutation log, which replays deterministically.
//
// Runs are deterministic by default (fixed seed) so CI is stable; set
// BATON_FUZZ_SEED to explore a different trajectory, and BATON_FUZZ_ROUNDS
// to run longer soaks.

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"testing"

	"github.com/conductorone/baton-sdk/pkg/logging"
	"github.com/stretchr/testify/require"
)

// ghFuzzView is a locked copy of the mock org's mutable state, used to
// pick valid mutation targets without racing the handler.
type ghFuzzView struct {
	memberIDs []int64 // non-admin
	adminIDs  []int64
}

func (m *mockGitHubOrg) fuzzView() ghFuzzView {
	m.mu.Lock()
	defer m.mu.Unlock()
	var v ghFuzzView
	for id, mem := range m.members {
		if mem.Admin {
			v.adminIDs = append(v.adminIDs, id)
		} else {
			v.memberIDs = append(v.memberIDs, id)
		}
	}
	sort.Slice(v.memberIDs, func(i, j int) bool { return v.memberIDs[i] < v.memberIDs[j] })
	sort.Slice(v.adminIDs, func(i, j int) bool { return v.adminIDs[i] < v.adminIDs[j] })
	return v
}

func (v ghFuzzView) allIDs() []int64 {
	return append(append([]int64{}, v.memberIDs...), v.adminIDs...)
}

type ghFuzzOp struct {
	name  string
	ready func(v ghFuzzView) bool
	apply func(f *ghFuzzRun, v ghFuzzView)
}

type ghFuzzRun struct {
	t   *testing.T
	m   *mockGitHubOrg
	rng *rand.Rand
	log []string
}

func (f *ghFuzzRun) note(format string, args ...any) {
	f.log = append(f.log, fmt.Sprintf(format, args...))
}

func (f *ghFuzzRun) pickID(ids []int64) int64 {
	return ids[f.rng.Intn(len(ids))]
}

// freshID picks an unused id anywhere in [1, 5000]: low ids fuzz
// mid-collection insertion (page-boundary shifts), high ids fuzz the
// append-at-end / full-tail-probe path.
func (f *ghFuzzRun) freshID(v ghFuzzView) int64 {
	used := map[int64]bool{}
	for _, id := range v.allIDs() {
		used[id] = true
	}
	for {
		id := int64(1 + f.rng.Intn(5000))
		if !used[id] {
			return id
		}
	}
}

func ghFuzzOps() []ghFuzzOp {
	return []ghFuzzOp{
		{
			name:  "add-member",
			ready: func(v ghFuzzView) bool { return true },
			apply: func(f *ghFuzzRun, v ghFuzzView) {
				id := f.freshID(v)
				f.m.addMember(id, false)
				f.note("add-member %d", id)
			},
		},
		{
			name:  "add-admin",
			ready: func(v ghFuzzView) bool { return true },
			apply: func(f *ghFuzzRun, v ghFuzzView) {
				id := f.freshID(v)
				f.m.addMember(id, true)
				f.note("add-admin %d", id)
			},
		},
		{
			name:  "remove-member",
			ready: func(v ghFuzzView) bool { return len(v.memberIDs) > 1 },
			apply: func(f *ghFuzzRun, v ghFuzzView) {
				id := f.pickID(v.memberIDs)
				f.m.removeMember(id)
				f.note("remove-member %d", id)
			},
		},
		{
			name:  "remove-admin",
			ready: func(v ghFuzzView) bool { return len(v.adminIDs) > 1 },
			apply: func(f *ghFuzzRun, v ghFuzzView) {
				id := f.pickID(v.adminIDs)
				f.m.removeMember(id)
				f.note("remove-admin %d", id)
			},
		},
		{
			name:  "promote",
			ready: func(v ghFuzzView) bool { return len(v.memberIDs) > 1 },
			apply: func(f *ghFuzzRun, v ghFuzzView) {
				id := f.pickID(v.memberIDs)
				f.m.setAdmin(id, true)
				f.note("promote %d", id)
			},
		},
		{
			name:  "demote",
			ready: func(v ghFuzzView) bool { return len(v.adminIDs) > 1 },
			apply: func(f *ghFuzzRun, v ghFuzzView) {
				id := f.pickID(v.adminIDs)
				f.m.setAdmin(id, false)
				f.note("demote %d", id)
			},
		},
		{
			// A login change perturbs the page's bytes (forcing a 200 and
			// a fresh fetch) without changing any grant row — the warm and
			// control outputs must still match exactly.
			name:  "rename",
			ready: func(v ghFuzzView) bool { return len(v.memberIDs)+len(v.adminIDs) > 0 },
			apply: func(f *ghFuzzRun, v ghFuzzView) {
				ids := v.allIDs()
				id := ids[f.rng.Intn(len(ids))]
				login := fmt.Sprintf("renamed-%d-%d", id, f.rng.Intn(1000))
				f.m.renameMember(id, login)
				f.note("rename %d -> %s", id, login)
			},
		},
	}
}

func ghFuzzEnvInt(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return def
}

func TestGitHubSourceCacheChurnFuzz(t *testing.T) {
	for _, workers := range []int{0, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			runGitHubChurnFuzz(t, workers)
		})
	}
}

func runGitHubChurnFuzz(t *testing.T, workers int) {
	ctx, err := logging.Init(t.Context())
	require.NoError(t, err)

	seed := int64(ghFuzzEnvInt("BATON_FUZZ_SEED", 20260711))
	rounds := ghFuzzEnvInt("BATON_FUZZ_ROUNDS", 8)
	t.Logf("churn fuzz: seed=%d rounds=%d workers=%d (override with BATON_FUZZ_SEED / BATON_FUZZ_ROUNDS)", seed, rounds, workers)

	mock := newMockGitHubOrg(t)

	// Seed org: ~2.5 member pages plus admins, so boundary shifts and the
	// tail page are in play from round zero.
	for i := int64(1); i <= 230; i++ {
		mock.addMember(i*3, false) // gaps leave room for mid-list inserts
	}
	for i := int64(1); i <= 5; i++ {
		mock.addMember(4000+i, true)
	}

	h := newGHSyncHarness(ctx, t, mock)
	h.workers = workers
	f := &ghFuzzRun{t: t, m: mock, rng: rand.New(rand.NewSource(seed))}
	ops := ghFuzzOps()

	prev := h.runSync("fuzz-cold", "")

	for round := 1; round <= rounds; round++ {
		nOps := 1 + f.rng.Intn(3)
		f.log = f.log[:0]
		for i := 0; i < nOps; i++ {
			// Re-view after each mutation so ops in the same round compose
			// against current state, exactly as real churn would.
			v := mock.fuzzView()
			var ready []ghFuzzOp
			for _, op := range ops {
				if op.ready(v) {
					ready = append(ready, op)
				}
			}
			require.NotEmpty(t, ready)
			ready[f.rng.Intn(len(ready))].apply(f, v)
		}

		// ~1 round in 6 also evicts every ETag, so degradation to cold is
		// fuzzed IN COMBINATION with churn, not only in isolation.
		if f.rng.Intn(6) == 0 {
			mock.evictEtags()
			f.note("evict-etags")
		}

		warm := h.runSync(fmt.Sprintf("fuzz-warm-%02d", round), prev)
		control := h.runSync(fmt.Sprintf("fuzz-control-%02d", round), "")

		wSnap := h.snapshot(warm)
		cSnap := h.snapshot(control)
		if !ghAssertSnapshotsEqual(t, cSnap, wSnap) {
			t.Fatalf("round %d diverged (seed=%d); mutations this round:\n  %s",
				round, seed, ghJoinLines(f.log))
		}
		prev = warm
	}
}

func ghAssertSnapshotsEqual(t *testing.T, control, warm map[string]string) bool {
	t.Helper()
	ok := true
	for k, cv := range control {
		wv, found := warm[k]
		if !found {
			t.Errorf("warm sync MISSING %s", k)
			ok = false
		} else if wv != cv {
			t.Errorf("warm sync DIFFERS at %s:\n  control: %s\n  warm:    %s", k, cv, wv)
			ok = false
		}
	}
	for k := range warm {
		if _, found := control[k]; !found {
			t.Errorf("warm sync EXTRA %s: %s", k, warm[k])
			ok = false
		}
	}
	return ok
}

func ghJoinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n  "
		}
		out += l
	}
	return out
}
