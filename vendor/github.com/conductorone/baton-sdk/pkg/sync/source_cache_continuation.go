package sync //nolint:revive,nolintlint // we can't change the package name for backwards compatibility

// Syncer side of the source-cache lookup continuation (ask/answer). On
// single-shot transports (gRPC-over-Lambda) the connector cannot call the
// lookup service mid-request, so it answers a list RPC with a
// SourceCacheLookupAsk instead of rows; the syncer resolves the queries
// against its LOCAL previous-sync store — the same store replay copies
// from, so lookup and replay can never disagree — and re-invokes the same
// request with SourceCacheLookupAnswers attached. See
// docs/tasks/source-cache-lambda-lookup.md and the annotation contract in
// proto/c1/connector/v2/annotation_source_cache.proto.

import (
	"context"
	"fmt"
	stdsync "sync"

	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/sourcecache"
)

const (
	// sourceCacheBounceCap bounds consecutive asks for the SAME request
	// (same page token; only the answers annotation differs between
	// re-invokes). A connector that keeps asking without progressing is
	// broken (most commonly: swallowing ErrLookupDeferred and re-asking
	// for scopes it already "handled"), and silence would be the
	// stale-data failure mode — so fail loudly.
	//
	// Deliberately NOT per action: a multi-page action that asks once per
	// page (e.g. a delta planner pre-resolving each planning page's chunk
	// scopes) bounces once per page with monotonic progress — every
	// NextPageToken advance is a new request and resets the counter.
	sourceCacheBounceCap = 4

	// sourceCacheAnswerBudget caps the FOUND-etag payload attached to one
	// re-invoke, keeping the request under single-shot transport payload
	// limits (Lambda invokes cap at 6MB; the dual-encoded frame at 5MiB).
	// Not-found answers are always complete for the queried set — only
	// found answers with large etags are dropped, and a dropped answer is
	// ABSENT (re-askable, subject to the cap), never a false not-found.
	sourceCacheAnswerBudget = 2 << 20
)

// continuationStats accumulates ask/answer counters across a sync for the
// sync-complete log line and bounce-cap diagnostics. Per-op-kind bounce
// counts let a rollout review distinguish planner asks (expected, one per
// planning page) from per-row asks (a batching opportunity).
type continuationStats struct {
	mu          stdsync.Mutex
	requests    int // RPCs that bounced at least once
	bounces     int
	bouncesByOp map[string]int
	asked       int
	found       int
	notFound    int
	truncated   int
	capFailures int
}

func (c *continuationStats) record(op string, bounces, asked, found, notFound, truncated int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if bounces > 0 {
		c.requests++
		if c.bouncesByOp == nil {
			c.bouncesByOp = map[string]int{}
		}
		c.bouncesByOp[op] += bounces
	}
	c.bounces += bounces
	c.asked += asked
	c.found += found
	c.notFound += notFound
	c.truncated += truncated
}

func (c *continuationStats) recordCapFailure() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capFailures++
}

// logTotals emits the continuation counters when any bounces happened.
func (c *continuationStats) logTotals(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bounces == 0 && c.capFailures == 0 {
		return
	}
	fields := []zap.Field{
		zap.Int("requests_bounced", c.requests),
		zap.Int("bounces", c.bounces),
		zap.Int("scopes_asked", c.asked),
		zap.Int("answered_found", c.found),
		zap.Int("answered_not_found", c.notFound),
		zap.Int("answers_truncated", c.truncated),
		zap.Int("bounce_cap_failures", c.capFailures),
	}
	for op, n := range c.bouncesByOp {
		fields = append(fields, zap.Int("bounces_"+op, n))
	}
	ctxzap.Extract(ctx).Info("source-cache lookup continuation totals", fields...)
}

// listAttempt is one list-RPC attempt as observed by the continuation
// loop: enough of the response to detect and validate an ask.
type listAttempt struct {
	annos     annotations.Annotations
	rows      int
	nextToken string
}

// withSourceCacheContinuation drives the ask/answer loop around one list
// RPC. issue performs the RPC with extra request annotations (the lookup
// offer, plus accumulated answers on re-invokes) and reports the response
// surface; the loop returns once a response carries no ask — that final
// response is the one the caller processes.
//
// The offer is attached only when the syncer can actually answer (warm
// previous-sync lookup). Old or cold syncers never send it, and a
// compliant connector never asks without it — that pairing is what makes
// version skew degrade to a cold sync instead of a misread response.
func (s *syncer) withSourceCacheContinuation(ctx context.Context, op string, issue func(extra annotations.Annotations) (listAttempt, error)) error {
	l := ctxzap.Extract(ctx)

	warm := s.sourceCache.prev != nil
	extra := annotations.Annotations{}
	if warm {
		extra.Update(&v2.SourceCacheLookupOffer{})
	}

	// Answers accumulate across bounces: the connector re-executes from
	// scratch each phase, so every re-invoke must carry the union of all
	// resolved queries, in first-resolved order (deterministic requests).
	var ordered []sourcecache.Answer
	seen := map[sourcecache.Query]bool{}

	asked, found, notFound, truncated := 0, 0, 0, 0
	for bounce := 0; ; bounce++ {
		attempt, err := issue(extra)
		if err != nil {
			return err
		}

		ask := &v2.SourceCacheLookupAsk{}
		hasAsk, err := attempt.annos.Pick(ask)
		if err != nil {
			return fmt.Errorf("%s: error parsing source-cache lookup ask: %w", op, err)
		}
		if !hasAsk {
			s.sourceCache.contStats.record(op, bounce, asked, found, notFound, truncated)
			return nil
		}

		// Ask legality. Failing loudly here is deliberate: every branch
		// is a connector bug that would otherwise surface as silently
		// wrong data or an unexplained stall.
		if !warm {
			return fmt.Errorf("%s: connector sent a source-cache lookup ask on a request that carried no offer (connector must gate asks on SourceCacheLookupOffer)", op)
		}
		if attempt.rows > 0 || attempt.nextToken != "" ||
			attempt.annos.Contains(&v2.SourceCacheScope{}) || attempt.annos.Contains(&v2.SourceCacheReplay{}) ||
			attempt.annos.Contains(&v2.SpawnCursors{}) {
			return fmt.Errorf("%s: source-cache lookup ask response must carry ONLY the ask: "+
				"no rows, no next page token, no scope/replay annotations, no spawned cursors "+
				"(spawn on the re-invoked request's real response instead)", op)
		}
		if bounce >= sourceCacheBounceCap {
			s.sourceCache.contStats.recordCapFailure()
			return fmt.Errorf("%s: source-cache lookup bounce cap (%d) exceeded for one request; "+
				"connector kept asking without progressing (%d scopes still unresolved) — "+
				"check for swallowed ErrLookupDeferred or nondeterministic scope computation",
				op, sourceCacheBounceCap, len(ask.GetQueries()))
		}

		queries, err := sourcecache.QueriesFromProto(ask)
		if err != nil {
			return fmt.Errorf("%s: invalid source-cache lookup ask: %w", op, err)
		}

		budget := sourceCacheAnswerBudget
		for _, a := range ordered {
			budget -= len(a.ETag)
		}
		newAsked, newFound, newNotFound, newTruncated := 0, 0, 0, 0
		for _, q := range queries {
			if seen[q] {
				continue
			}
			newAsked++
			entry, ok, err := s.sourceCache.lookup.LookupPreviousSourceCache(ctx, q.RowKind, q.ScopeHash)
			if err != nil {
				return fmt.Errorf("%s: resolving source-cache lookup ask: %w", op, err)
			}
			if !ok {
				seen[q] = true
				ordered = append(ordered, sourcecache.Answer{Query: q, Found: false})
				newNotFound++
				continue
			}
			if len(entry.ETag) > budget {
				// Dropped to budget: the query stays ABSENT from the
				// answers (re-askable), never a false not-found.
				newTruncated++
				continue
			}
			budget -= len(entry.ETag)
			seen[q] = true
			ordered = append(ordered, sourcecache.Answer{Query: q, Found: true, ETag: entry.ETag})
			newFound++
		}
		asked += newAsked
		found += newFound
		notFound += newNotFound
		truncated += newTruncated

		if newAsked == 0 {
			// Every query was already answered on the request the
			// connector just saw; re-invoking cannot make progress.
			return fmt.Errorf("%s: connector re-asked only already-answered scopes (%d queries); connector lookup handling is broken", op, len(queries))
		}

		extra.Update(sourcecache.AnswersProto(ordered))

		l.Debug("source-cache lookup bounce",
			zap.String("op", op),
			zap.Int("bounce", bounce+1),
			zap.Int("asked", newAsked),
			zap.Int("found", newFound),
			zap.Int("not_found", newNotFound),
			zap.Int("truncated_to_budget", newTruncated),
		)
	}
}
