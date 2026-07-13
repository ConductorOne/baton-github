package connector

// Source-cache replay for GitHub conditional requests.
//
// GitHub is the conditional-request model: a warm sync re-presents the
// previous sync's ETag via If-None-Match; a 304 means "this page is
// byte-identical to last sync" and the SDK replays the page's previous
// rows from the prior c1z. Authorized 304s do not count against the
// primary rate limit, so warm syncs of unchanged data are nearly free.
//
// Scope model: ONE SCOPE PER PAGE, not one scope per collection.
//
// The handoff design preferred a whole-collection scope validated by
// page 1's ETag, contingent on an empirical property: "a change anywhere
// in the collection changes page 1's ETag". For GitHub member listings
// that property should be presumed FALSE — members are returned in
// ascending user-id order, so a newly added member lands on the LAST
// page and page 1's body-hash ETag does not move. A collection scope
// keyed on page 1 would silently drop new members on warm syncs. Until
// that property is verified live per endpoint, we use the documented
// fallback: store every page's ETag and revalidate each page.
//
// Per-page soundness: a 304 for page k attests that page k's bytes are
// identical to the stored rows for that page's scope, so replaying them
// is exact. Pages whose contents shifted (insert/delete before them)
// answer 200 and are re-fetched fresh. The union of live pages is
// duplicate-free because all pages are read from the live listing in one
// sync.
//
// Chain continuation on 304: GitHub 304 responses carry no usable Link
// header, so "is there a page k+1" cannot come from the response. The
// validator therefore encodes the page's row count. A full page (row
// count == per_page) means a next page MAY exist and must be visited —
// including the append-past-a-full-tail case, where the probe of page
// k+1 either finds fresh rows (200) or an empty listing (200 []) that
// ends the chain. A partial page ends the chain, and any append onto a
// partial page changes its bytes, forcing a 200.
//
// Scope signatures are versioned (bump scopeSigVersion when the row
// shape changes so every old scope misses at once) and include the auth
// identity, because GitHub ETag visibility is credential-specific: two
// different tokens must never share scopes.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/sourcecache"
	"github.com/google/go-github/v69/github"
)

// scopeSigVersion invalidates ALL previously stored scopes when bumped.
// Bump it whenever the rows emitted for a page change shape (new grant
// fields, different grant ids, different page size, ...).
const scopeSigVersion = "v1"

// validatorVersion namespaces the encoded validator format.
const validatorVersion = "v1"

// pageScopeSig is the canonical pre-image for one page's scope hash. The
// request URL carries host (GHE instance), path (org login), and the
// full query shape (role, per_page, page); authScope isolates
// credentials with different visibility.
func pageScopeSig(kind string, authScope string, requestURL string) string {
	return scopeSigVersion + "|gh|" + kind + "|auth=" + authScope + "|GET " + requestURL
}

// encodeValidator packs the page's verbatim ETag together with the row
// count the page produced. The row count drives chain continuation on a
// 304 (full page => a following page may exist). The whole string is
// opaque to the SDK.
func encodeValidator(etag string, rows int) string {
	return validatorVersion + "|n=" + strconv.Itoa(rows) + "|" + etag
}

// decodeValidator reverses encodeValidator. ok=false (unknown version,
// malformed) must degrade to a plain fetch, never an error.
func decodeValidator(v string) (etag string, rows int, ok bool) {
	rest, found := strings.CutPrefix(v, validatorVersion+"|n=")
	if !found {
		return "", 0, false
	}
	nStr, etag, found := strings.Cut(rest, "|")
	if !found {
		return "", 0, false
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n < 0 {
		return "", 0, false
	}
	return etag, n, true
}

// Validator resolution states carried by pagedListCursor. Unresolved
// (empty) means "do a lookup for this page"; the other two mean the
// origin's batched lookup already resolved it, so the page does NO lookup
// of its own — on the Lambda ask/answer continuation that is the
// difference between zero bounces and one bounce per page.
const (
	// cursorValidatorHit: Etag holds the stored validator verbatim.
	cursorValidatorHit = "hit"
	// cursorValidatorMiss: the previous sync has no entry for this page
	// (known from the origin's contiguous-chain walk); fetch cold.
	cursorValidatorMiss = "miss"
)

// pagedListCursor is the bag token for a source-cached paged listing.
// Page is 1-based; page 0/empty token means page 1.
//
// Horizon is the spawn horizon: the last page of this collection covered
// by a spawned sibling cursor this sync (see resolveStoredPageChain).
// Pages ≤ Horizon are sibling-owned, so a page's own chain never
// continues into them — without this clamp the origin's chain and a
// sibling would both serve the same page. It rides the token (not a
// lookup re-derivation) so it is exact even if the stored manifest has
// gaps, and survives suspend/resume on any worker.
//
// Resolution/Etag carry the page's validator resolution from the
// origin's batched lookup (see the constants above). Carrying validators
// through connector-authored tokens within one sync is blessed by the
// source-cache contract: the replay invariant is "validator came from
// THIS SYNC's lookup", not "from this action's lookup". Old tokens
// without these fields decode as unresolved and behave exactly as
// before.
type pagedListCursor struct {
	Page       int    `json:"page"`
	Horizon    int    `json:"horizon,omitempty"`
	Resolution string `json:"res,omitempty"`
	Etag       string `json:"etag,omitempty"`
}

func parsePagedListCursor(token string) (pagedListCursor, error) {
	if token == "" {
		return pagedListCursor{Page: 1}, nil
	}
	var c pagedListCursor
	if err := json.Unmarshal([]byte(token), &c); err != nil {
		return pagedListCursor{}, fmt.Errorf("github-connector: bad page cursor %q: %w", token, err)
	}
	if c.Page < 1 {
		c.Page = 1
	}
	return c, nil
}

func (c pagedListCursor) marshal() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// conditionalPage is the outcome of one source-cached page fetch.
type conditionalPage struct {
	// Users holds the page's fresh rows. Empty on a 304 replay.
	Users []*github.User
	// Replayed is true when the page answered 304 and the SDK will copy
	// the previous sync's rows for this scope.
	Replayed bool
	// NextPage is the 1-based page to fetch next; 0 ends the chain.
	NextPage int
	// Annos carries rate-limit plus the page's SourceCacheScope or
	// SourceCacheReplay annotation.
	Annos annotations.Annotations
	// Resp is the raw response for error classification (may be nil).
	Resp *github.Response
}

// listUsersPageConditional fetches one page of a user-list endpoint with
// source-cache revalidation. pageURL must be byte-stable across syncs
// for the same logical page (fixed query-param order, explicit page and
// per_page). kind names the endpoint family within the scope signature.
// perPage must equal the per_page baked into pageURL; it drives the
// full-page chain-continuation check on a 304. cursor supplies the page
// number, the sibling horizon clamp, and — when the origin's batched
// walk already resolved it — the page's validator, in which case no
// lookup happens here at all (zero ask/answer bounces on the Lambda
// continuation). An unresolved cursor (legacy checkpointed token) falls
// back to a single lookup.
func listUsersPageConditional(
	ctx context.Context,
	client *github.Client,
	lookup sourcecache.Lookup,
	authScope string,
	kind string,
	pageURL string,
	cursor pagedListCursor,
	perPage int,
) (*conditionalPage, error) {
	req, err := client.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	page, horizon := cursor.Page, cursor.Horizon

	scope := sourcecache.HashScope(pageScopeSig(kind, authScope, req.URL.String()))
	if lookup == nil {
		lookup = sourcecache.NoopLookup{}
	}
	storedValidator, found := "", false
	switch cursor.Resolution {
	case cursorValidatorHit:
		storedValidator, found = cursor.Etag, true
	case cursorValidatorMiss:
		// Known cold; skip the lookup.
	default:
		entry, entryFound, err := lookup.LookupPreviousSourceCache(ctx, sourcecache.RowKindGrants, scope)
		if err != nil {
			// May wrap sourcecache.ErrLookupDeferred (ask/answer
			// continuation); must propagate unswallowed.
			return nil, err
		}
		if entryFound {
			storedValidator, found = entry.ETag, true
		}
	}
	prevEtag, prevRows := "", 0
	validatorOK := false
	if found {
		prevEtag, prevRows, validatorOK = decodeValidator(storedValidator)
	}
	// A hit with a decodable validator makes the request conditional; any
	// miss or malformed validator degrades to a plain (cold) fetch.
	conditional := validatorOK && prevEtag != ""
	if conditional {
		req.Header.Set("If-None-Match", prevEtag)
	}

	resp, err := client.BareDo(ctx, req)
	_, annos, parseErr := parseResp(resp)
	if parseErr != nil {
		return nil, fmt.Errorf("github-connector: failed to parse response: %w", parseErr)
	}

	if resp != nil && resp.StatusCode == http.StatusNotModified {
		if !conditional {
			// Invariant guard: a replay is only legal for a scope whose
			// validator came from this sync's lookup.
			return nil, fmt.Errorf("github-connector: 304 for %s without a source-cache lookup hit", pageURL)
		}
		next := 0
		if prevRows >= perPage {
			// The stored page was full, so a following page may exist (it
			// did last sync, or an append created one since). Visit it: a
			// stale hint costs one request against the live listing, which
			// answers with fresh rows or an empty 200 that ends the chain.
			next = page + 1
		}
		if next != 0 && next <= horizon {
			// A spawned sibling owns that page; chaining into it would
			// serve it twice.
			next = 0
		}
		annos.Append(v2.SourceCacheReplay_builder{
			ScopeHash: scope,
			Etag:      storedValidator,
		}.Build())
		return &conditionalPage{Replayed: true, NextPage: next, Annos: annos, Resp: resp}, nil
	}
	if err != nil {
		return &conditionalPage{Resp: resp}, wrapGitHubError(err, resp, "github-connector: failed to list "+kind)
	}
	defer resp.Body.Close()

	var users []*github.User
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return &conditionalPage{Resp: resp}, fmt.Errorf("github-connector: failed to decode %s page: %w", kind, err)
	}

	// A 200 with zero rows still carries the scope annotation: the etag
	// must survive to the next sync (an empty page is replayable too).
	etag := resp.Header.Get("ETag")
	scopeEtag := ""
	if etag != "" {
		scopeEtag = encodeValidator(etag, len(users))
	}
	annos.Append(v2.SourceCacheScope_builder{
		ScopeHash: scope,
		Etag:      scopeEtag,
	}.Build())
	next := resp.NextPage
	if next != 0 && next <= horizon {
		// Sibling-owned page (see the 304 branch): the sibling
		// revalidates it independently, fresh or replayed.
		next = 0
	}
	return &conditionalPage{Users: users, NextPage: next, Annos: annos, Resp: resp}, nil
}

// storedChainBatchSize is how many pages one LookupMany batch covers.
// On the Lambda ask/answer continuation each batch costs one bounce
// (bounce cap: 4), so the batch size bounds the resolvable chain:
// 3 batches × 256 pages ≈ 76k members per role before the origin action
// would fail the cap — far beyond any real GitHub collection. Validators
// are ~150 bytes encoded, so a full batch is well under the transport's
// answer budget (no truncation re-asks).
const storedChainBatchSize = 256

// storedChainBatchCap bounds the total walk; guards a pathological
// manifest (and stays a batch short of the continuation's bounce cap).
const storedChainBatchCap = 3

// resolvedPage is one page's validator resolution from the origin's
// batched walk, embeddable into a pagedListCursor.
type resolvedPage struct {
	Page int
	// Found + Validator mirror the manifest answer for the page's scope.
	Found     bool
	Validator string
}

// resolveStoredPageChain resolves the collection's stored page chain in
// LookupMany batches, starting at page 1. It returns the resolution for
// every page 1..horizon+1 that was queried, and the horizon: the last
// page of the CONTIGUOUS stored chain (0 = nothing stored, cold sync).
//
// One call resolves everything the sync needs for this collection —
// page 1's own validator, every sibling's validator (carried to the
// spawned cursors so siblings do no lookups of their own), and the
// horizon. On the Lambda continuation the whole walk costs one ask
// bounce per batch; in-process it is a loop of local point-reads.
func resolveStoredPageChain(
	ctx context.Context,
	client *github.Client,
	lookup sourcecache.Lookup,
	authScope string,
	kind string,
	pageURLFor func(page int) string,
) (map[int]resolvedPage, int, error) {
	resolved := map[int]resolvedPage{}
	if lookup == nil {
		lookup = sourcecache.NoopLookup{}
	}

	horizon := 0
	chainBroken := false
	for batch := 0; batch < storedChainBatchCap && !chainBroken; batch++ {
		first := batch*storedChainBatchSize + 1
		queries := make([]sourcecache.Query, 0, storedChainBatchSize)
		pages := make([]int, 0, storedChainBatchSize)
		for p := first; p < first+storedChainBatchSize; p++ {
			req, err := client.NewRequest(http.MethodGet, pageURLFor(p), nil)
			if err != nil {
				return nil, 0, err
			}
			queries = append(queries, sourcecache.Query{
				RowKind:   sourcecache.RowKindGrants,
				ScopeHash: sourcecache.HashScope(pageScopeSig(kind, authScope, req.URL.String())),
			})
			pages = append(pages, p)
		}
		answers, err := sourcecache.LookupMany(ctx, lookup, queries)
		if err != nil {
			// May wrap sourcecache.ErrLookupDeferred; propagate.
			return nil, 0, err
		}
		for i, a := range answers {
			p := pages[i]
			resolved[p] = resolvedPage{Page: p, Found: a.Found, Validator: a.ETag}
			if !a.Found && !chainBroken {
				chainBroken = true
			}
			if a.Found && !chainBroken {
				horizon = p
			}
		}
	}
	return resolved, horizon, nil
}
