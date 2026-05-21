package sourcecache

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const keyVersion = "v2"

var scopeHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

type RowKind string

const (
	RowKindResources    RowKind = "resources"
	RowKindEntitlements RowKind = "entitlements"
	RowKindGrants       RowKind = "grants"
)

type Entry struct {
	Key  string
	ETag string
}

type Lookup interface {
	LookupPreviousSourceCache(ctx context.Context, rowKind RowKind, scopeHashHex string) (Entry, bool, error)
}

type SetLookup interface {
	SetSourceCache(ctx context.Context, lookup Lookup)
}

type NoopLookup struct{}

func (NoopLookup) LookupPreviousSourceCache(context.Context, RowKind, string) (Entry, bool, error) {
	return Entry{}, false, nil
}

func ValidateRowKind(rowKind RowKind) error {
	switch rowKind {
	case RowKindResources, RowKindEntitlements, RowKindGrants:
		return nil
	default:
		return fmt.Errorf("invalid source cache row kind: %q", rowKind)
	}
}

func ValidateScopeHash(scopeHashHex string) error {
	if !scopeHashRe.MatchString(scopeHashHex) {
		return fmt.Errorf("invalid source cache scope hash: %q", scopeHashHex)
	}
	return nil
}

func BuildKey(scopeHashHex string, etag string) (string, error) {
	if err := ValidateScopeHash(scopeHashHex); err != nil {
		return "", err
	}
	if etag == "" {
		return "", errors.New("source cache etag is required")
	}
	encodedETag := base64.RawURLEncoding.EncodeToString([]byte(etag))
	return fmt.Sprintf("%s:%s:%s", keyVersion, scopeHashHex, encodedETag), nil
}

func ParseKey(key string) (string, string, error) {
	parts := strings.Split(key, ":")
	if len(parts) != 3 {
		return "", "", fmt.Errorf("invalid source cache key format: %q", key)
	}
	if parts[0] != keyVersion {
		return "", "", fmt.Errorf("unsupported source cache key version: %q", parts[0])
	}
	scopeHashHex := parts[1]
	if err := ValidateScopeHash(scopeHashHex); err != nil {
		return "", "", err
	}
	if parts[2] == "" {
		return "", "", errors.New("source cache key etag segment is required")
	}
	etagBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", "", fmt.Errorf("invalid source cache etag encoding: %w", err)
	}
	etag := string(etagBytes)
	if etag == "" {
		return "", "", errors.New("source cache etag is required")
	}
	return scopeHashHex, etag, nil
}
