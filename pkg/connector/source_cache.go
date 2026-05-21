package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/sourcecache"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

const sourceCacheProducerVersion = "baton-github-source-cache-v1"

type sourceCacheState struct {
	scopeHash string
	prev      sourcecache.Entry
	found     bool
}

func sourceScopeHash(parts ...string) string {
	scope := strings.Join(append([]string{sourceCacheProducerVersion}, parts...), "\x00")
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:])
}

func prepareSourceCache(ctx context.Context, opts resource.SyncOpAttrs, rowKind sourcecache.RowKind, parts ...string) (context.Context, sourceCacheState, error) {
	state := sourceCacheState{scopeHash: sourceScopeHash(parts...)}
	if opts.SourceCache == nil {
		ctxzap.Extract(ctx).Warn("source cache lookup unavailable; SyncOpAttrs.SourceCache is nil",
			zap.String("row_kind", string(rowKind)),
			zap.String("source_scope_hash", state.scopeHash),
		)
		return ctx, state, nil
	}
	if _, ok := opts.SourceCache.(sourcecache.NoopLookup); ok {
		ctxzap.Extract(ctx).Warn("source cache lookup unavailable; SyncOpAttrs.SourceCache is noop",
			zap.String("row_kind", string(rowKind)),
			zap.String("source_scope_hash", state.scopeHash),
		)
		return ctx, state, nil
	}

	entry, found, err := opts.SourceCache.LookupPreviousSourceCache(ctx, rowKind, state.scopeHash)
	if err != nil {
		ctxzap.Extract(ctx).Warn("source cache lookup failed; falling back to unconditional GitHub request",
			zap.String("row_kind", string(rowKind)),
			zap.String("source_scope_hash", state.scopeHash),
			zap.Error(err),
		)
		return ctx, state, nil
	}
	if !found {
		return ctx, state, nil
	}

	state.prev = entry
	state.found = true
	return ctx, state, nil
}

func conditionalGitHubGet(ctx context.Context, client *github.Client, endpoint string, query url.Values, state sourceCacheState, out any) (*github.Response, error) {
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}

	req, err := client.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if state.found && state.prev.ETag != "" {
		req.Header.Set("If-None-Match", state.prev.ETag)
	}

	return client.Do(ctx, req, out)
}

func githubPath(parts ...string) string {
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		escaped = append(escaped, url.PathEscape(part))
	}
	return strings.Join(escaped, "/")
}

func githubListValues(page int, perPage int) url.Values {
	v := url.Values{}
	if page > 0 {
		v.Set("page", fmt.Sprintf("%d", page))
	}
	if perPage > 0 {
		v.Set("per_page", fmt.Sprintf("%d", perPage))
	}
	return v
}

func sourceCacheReplayAnnotations(state sourceCacheState) (annotations.Annotations, error) {
	if !state.found || state.prev.Key == "" {
		return nil, errors.New("source cache replay requested without a previous cache key")
	}
	return annotations.New(&v2.SourceCacheReplay{Key: state.prev.Key}), nil
}

func addSourceCacheKeyAnnotation(annos annotations.Annotations, state sourceCacheState, resp *github.Response, rowCount int) (annotations.Annotations, error) {
	if rowCount == 0 || resp == nil || state.scopeHash == "" {
		return annos, nil
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		return annos, nil
	}

	key, err := sourcecache.BuildKey(state.scopeHash, etag)
	if err != nil {
		return annos, err
	}

	annos.Update(&v2.SourceCacheKey{Key: key})
	return annos, nil
}

func isSourceCacheNotModified(resp *github.Response, err error) bool {
	if resp != nil && resp.Response != nil && resp.StatusCode == http.StatusNotModified {
		return true
	}

	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotModified {
		return true
	}
	return false
}

func nextGitHubPageAfterReplay(page int) string {
	if page == 0 {
		return "2"
	}
	return fmt.Sprintf("%d", page+1)
}

func sortedOrgNames(orgs map[string]struct{}) []string {
	if len(orgs) == 0 {
		return nil
	}
	ret := make([]string, 0, len(orgs))
	for org := range orgs {
		ret = append(ret, org)
	}
	sort.Strings(ret)
	return ret
}
