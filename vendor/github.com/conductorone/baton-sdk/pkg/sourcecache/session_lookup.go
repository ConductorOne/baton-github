package sourcecache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/conductorone/baton-sdk/pkg/types/sessions"
)

const sessionLookupPrefix = "__baton_source_cache_lookup:"

type SessionLookup struct {
	session      sessions.SessionStore
	resourceType string
}

func NewSessionLookup(session sessions.SessionStore) Lookup {
	return NewSessionLookupForResourceType(session, "")
}

func NewSessionLookupForResourceType(session sessions.SessionStore, resourceType string) Lookup {
	if session == nil {
		return NoopLookup{}
	}
	return SessionLookup{session: session, resourceType: resourceType}
}

func SessionLookupKey(rowKind RowKind, scopeHashHex string) (string, error) {
	return SessionLookupKeyForResourceType(rowKind, scopeHashHex, "")
}

func SessionLookupKeyForResourceType(rowKind RowKind, scopeHashHex string, resourceType string) (string, error) {
	if err := ValidateRowKind(rowKind); err != nil {
		return "", err
	}
	if err := ValidateScopeHash(scopeHashHex); err != nil {
		return "", err
	}
	if resourceType != "" {
		return sessionLookupPrefix + string(rowKind) + ":" + scopeHashHex + ":" + resourceType, nil
	}
	return sessionLookupPrefix + string(rowKind) + ":" + scopeHashHex, nil
}

func ParseSessionLookupKey(key string) (RowKind, string, string, bool, error) {
	if !strings.HasPrefix(key, sessionLookupPrefix) {
		return "", "", "", false, nil
	}
	parts := strings.Split(strings.TrimPrefix(key, sessionLookupPrefix), ":")
	if len(parts) != 2 && len(parts) != 3 {
		return "", "", "", true, fmt.Errorf("invalid source cache session lookup key: %q", key)
	}
	rowKind := RowKind(parts[0])
	if err := ValidateRowKind(rowKind); err != nil {
		return "", "", "", true, err
	}
	scopeHashHex := parts[1]
	if err := ValidateScopeHash(scopeHashHex); err != nil {
		return "", "", "", true, err
	}
	resourceType := ""
	if len(parts) == 3 {
		resourceType = parts[2]
	}
	return rowKind, scopeHashHex, resourceType, true, nil
}

func EncodeEntry(entry Entry) ([]byte, error) {
	return json.Marshal(entry)
}

func DecodeEntry(data []byte) (Entry, error) {
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func (l SessionLookup) LookupPreviousSourceCache(ctx context.Context, rowKind RowKind, scopeHashHex string) (Entry, bool, error) {
	key, err := SessionLookupKeyForResourceType(rowKind, scopeHashHex, l.resourceType)
	if err != nil {
		return Entry{}, false, err
	}
	data, ok, err := l.session.Get(ctx, key)
	if err != nil {
		return Entry{}, false, err
	}
	if !ok {
		return Entry{}, false, nil
	}
	entry, err := DecodeEntry(data)
	if err != nil {
		return Entry{}, false, err
	}
	return entry, true, nil
}
