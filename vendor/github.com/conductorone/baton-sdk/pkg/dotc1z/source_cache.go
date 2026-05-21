package dotc1z

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/sourcecache"
	"github.com/doug-martin/goqu/v9"
)

const sourceCacheEntriesTableVersion = "1"
const sourceCacheEntriesTableName = "source_cache_entries"
const sourceCacheEntriesTableSchema = `
create table if not exists %s (
    id integer primary key,
    sync_id text not null,
    row_kind text not null,
    source_scope_hash text not null,
    source_cache_key text not null,
    source_etag text not null,
    discovered_at datetime not null
);
create unique index if not exists %s on %s (sync_id, row_kind, source_scope_hash);
create unique index if not exists %s on %s (sync_id, row_kind, source_cache_key);`

var sourceCacheEntries = (*sourceCacheEntriesTable)(nil)

type sourceCacheEntriesTable struct{}

func (s *sourceCacheEntriesTable) Name() string {
	return fmt.Sprintf("v%s_%s", s.Version(), sourceCacheEntriesTableName)
}

func (s *sourceCacheEntriesTable) Version() string {
	return sourceCacheEntriesTableVersion
}

func (s *sourceCacheEntriesTable) Schema() (string, []any) {
	return sourceCacheEntriesTableSchema, []any{
		s.Name(),
		fmt.Sprintf("idx_source_cache_entries_sync_kind_scope_v%s", s.Version()),
		s.Name(),
		fmt.Sprintf("idx_source_cache_entries_sync_kind_key_v%s", s.Version()),
		s.Name(),
	}
}

func (s *sourceCacheEntriesTable) Migrations(context.Context, *goqu.Database) (bool, error) {
	return false, nil
}

type SourceCacheStore interface {
	LookupPreviousSourceCache(ctx context.Context, rowKind sourcecache.RowKind, syncID string, scopeHashHex string) (sourcecache.Entry, bool, error)
	PutSourceCacheEntry(ctx context.Context, rowKind sourcecache.RowKind, syncID string, scopeHashHex string, sourceCacheKey string, sourceETag string, discoveredAt time.Time) error
	PutResources(ctx context.Context, sourceCacheKey string, resources ...*v2.Resource) error
	PutEntitlements(ctx context.Context, sourceCacheKey string, entitlements ...*v2.Entitlement) error
	PutGrants(ctx context.Context, sourceCacheKey string, grants ...*v2.Grant) error
	ReplaySourceCacheRows(ctx context.Context, from C1ZStore, rowKind sourcecache.RowKind, fromSyncID string, toSyncID string, sourceCacheKey string, discoveredAt time.Time) (int64, error)
	ReplaySourceCacheEntry(ctx context.Context, from C1ZStore, rowKind sourcecache.RowKind, fromSyncID string, toSyncID string, sourceCacheKey string, discoveredAt time.Time) error
}

func sourceCacheColumnMigration(ctx context.Context, db *goqu.Database, tableName string, indexName string) (bool, error) {
	migrated := false
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		"alter table %s add column source_cache_key text not null default ''",
		tableName,
	)); err != nil {
		if !isAlreadyExistsError(err) {
			return false, err
		}
	} else {
		migrated = true
	}

	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		"create index if not exists %s on %s (sync_id, source_cache_key) where source_cache_key != ''",
		indexName,
		tableName,
	)); err != nil {
		return false, err
	}

	return migrated, nil
}

type c1FileSourceCache struct{ c *C1File }

func (s c1FileSourceCache) LookupPreviousSourceCache(ctx context.Context, rowKind sourcecache.RowKind, syncID string, scopeHashHex string) (sourcecache.Entry, bool, error) {
	if err := sourcecache.ValidateRowKind(rowKind); err != nil {
		return sourcecache.Entry{}, false, err
	}
	if err := sourcecache.ValidateScopeHash(scopeHashHex); err != nil {
		return sourcecache.Entry{}, false, err
	}

	var key string
	var etag string
	err := s.c.db.QueryRowContext(ctx, fmt.Sprintf(
		`select source_cache_key, source_etag from %s where sync_id = ? and row_kind = ? and source_scope_hash = ?`,
		sourceCacheEntries.Name(),
	), syncID, string(rowKind), scopeHashHex).Scan(&key, &etag)
	if err != nil {
		if err == sql.ErrNoRows {
			return sourcecache.Entry{}, false, nil
		}
		return sourcecache.Entry{}, false, err
	}
	return sourcecache.Entry{Key: key, ETag: etag}, true, nil
}

func (s c1FileSourceCache) PutSourceCacheEntry(
	ctx context.Context,
	rowKind sourcecache.RowKind,
	syncID string,
	scopeHashHex string,
	sourceCacheKey string,
	sourceETag string,
	discoveredAt time.Time,
) error {
	if s.c.readOnly {
		return ErrReadOnly
	}
	if err := sourcecache.ValidateRowKind(rowKind); err != nil {
		return err
	}
	keyScopeHash, keyETag, err := sourcecache.ParseKey(sourceCacheKey)
	if err != nil {
		return err
	}
	if keyScopeHash != scopeHashHex {
		return fmt.Errorf("source cache key scope hash %q does not match entry scope hash %q", keyScopeHash, scopeHashHex)
	}
	if keyETag != sourceETag {
		return fmt.Errorf("source cache key etag does not match entry etag")
	}

	ds := s.c.db.Insert(sourceCacheEntries.Name()).Rows(goqu.Record{
		"sync_id":           syncID,
		"row_kind":          string(rowKind),
		"source_scope_hash": scopeHashHex,
		"source_cache_key":  sourceCacheKey,
		"source_etag":       sourceETag,
		"discovered_at":     discoveredAt.Format("2006-01-02 15:04:05.999999999"),
	}).OnConflict(goqu.DoUpdate("sync_id, row_kind, source_scope_hash", goqu.Record{
		"source_cache_key": goqu.I("EXCLUDED.source_cache_key"),
		"source_etag":      goqu.I("EXCLUDED.source_etag"),
		"discovered_at":    goqu.I("EXCLUDED.discovered_at"),
	})).Prepared(true)
	query, args, err := ds.ToSQL()
	if err != nil {
		return err
	}
	if _, err := s.c.db.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	s.c.dbUpdated = true
	return nil
}

func (s c1FileSourceCache) PutResources(ctx context.Context, sourceCacheKey string, resourcesToPut ...*v2.Resource) error {
	return s.c.putResourcesInternal(ctx, func(ctx context.Context, c *C1File, tableName string, extractFields func(*v2.Resource) (goqu.Record, error), resources ...*v2.Resource) error {
		return bulkPutConnectorObjectWithSourceCacheKey(ctx, c, tableName, sourceCacheKey, extractFields, resources...)
	}, resourcesToPut...)
}

func (s c1FileSourceCache) PutEntitlements(ctx context.Context, sourceCacheKey string, entitlementsToPut ...*v2.Entitlement) error {
	return s.c.putEntitlementsInternal(ctx, func(ctx context.Context, c *C1File, tableName string, extractFields func(*v2.Entitlement) (goqu.Record, error), entitlements ...*v2.Entitlement) error {
		return bulkPutConnectorObjectWithSourceCacheKey(ctx, c, tableName, sourceCacheKey, extractFields, entitlements...)
	}, entitlementsToPut...)
}

func (s c1FileSourceCache) PutGrants(ctx context.Context, sourceCacheKey string, grantsToPut ...*v2.Grant) error {
	return s.c.upsertGrantsWithSourceCacheKey(ctx, sourceCacheKey, grantsToPut...)
}

func (s c1FileSourceCache) ReplaySourceCacheRows(
	ctx context.Context,
	from C1ZStore,
	rowKind sourcecache.RowKind,
	fromSyncID string,
	toSyncID string,
	sourceCacheKey string,
	discoveredAt time.Time,
) (int64, error) {
	tableName, err := sourceCacheTableForRowKind(rowKind)
	if err != nil {
		return 0, err
	}
	return s.replaySourceCacheTable(ctx, from, tableName, fromSyncID, toSyncID, sourceCacheKey, discoveredAt)
}

func (s c1FileSourceCache) ReplaySourceCacheEntry(
	ctx context.Context,
	from C1ZStore,
	rowKind sourcecache.RowKind,
	fromSyncID string,
	toSyncID string,
	sourceCacheKey string,
	discoveredAt time.Time,
) error {
	if err := sourcecache.ValidateRowKind(rowKind); err != nil {
		return err
	}
	rows, err := s.replaySourceCacheTable(ctx, from, sourceCacheEntries.Name(), fromSyncID, toSyncID, sourceCacheKey, discoveredAt,
		func(sourceAlias string) string {
			return fmt.Sprintf(" and %s.row_kind = ?", sourceAlias)
		},
		string(rowKind),
	)
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("source cache entry not found for sync_id %q, row_kind %q, source_cache_key %q", fromSyncID, rowKind, sourceCacheKey)
	}
	return nil
}

type replayWhereFn func(sourceAlias string) string

func (s c1FileSourceCache) replaySourceCacheTable(
	ctx context.Context,
	from C1ZStore,
	tableName string,
	fromSyncID string,
	toSyncID string,
	sourceCacheKey string,
	discoveredAt time.Time,
	extraWhereAndArgs ...any,
) (int64, error) {
	if s.c.readOnly {
		return 0, ErrReadOnly
	}
	sourceFile, ok := AsSQLiteStore(from)
	if !ok {
		return 0, fmt.Errorf("source cache replay requires a sqlite-backed c1z store")
	}
	if sourceFile == s.c {
		rows, err := s.replaySourceCacheTableSQL(ctx, tableName, "main", "src", fromSyncID, toSyncID, sourceCacheKey, discoveredAt, extraWhereAndArgs...)
		if err != nil {
			return 0, err
		}
		s.c.dbUpdated = true
		return rows, nil
	}

	conn, err := s.c.rawDb.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	attachPath := strings.ReplaceAll(sourceFile.dbFilePath, "'", "''")
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("ATTACH '%s' AS source_cache_ref", attachPath)); err != nil {
		return 0, err
	}
	defer conn.ExecContext(ctx, "DETACH source_cache_ref") //nolint:errcheck // Best-effort cleanup.

	rows, err := s.replaySourceCacheTableSQLConn(ctx, conn, tableName, "source_cache_ref", "src", fromSyncID, toSyncID, sourceCacheKey, discoveredAt, extraWhereAndArgs...)
	if err != nil {
		return 0, err
	}
	s.c.dbUpdated = true
	return rows, nil
}

func (s c1FileSourceCache) replaySourceCacheTableSQL(
	ctx context.Context,
	tableName string,
	sourceSchema string,
	sourceAlias string,
	fromSyncID string,
	toSyncID string,
	sourceCacheKey string,
	discoveredAt time.Time,
	extraWhereAndArgs ...any,
) (int64, error) {
	conn, err := s.c.rawDb.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return s.replaySourceCacheTableSQLConn(ctx, conn, tableName, sourceSchema, sourceAlias, fromSyncID, toSyncID, sourceCacheKey, discoveredAt, extraWhereAndArgs...)
}

func (s c1FileSourceCache) replaySourceCacheTableSQLConn(
	ctx context.Context,
	conn *sql.Conn,
	tableName string,
	sourceSchema string,
	sourceAlias string,
	fromSyncID string,
	toSyncID string,
	sourceCacheKey string,
	discoveredAt time.Time,
	extraWhereAndArgs ...any,
) (int64, error) {
	columns, err := cloneTableColumns(ctx, conn, tableName)
	if err != nil {
		return 0, err
	}

	columnList := make([]string, 0, len(columns))
	selectList := make([]string, 0, len(columns))
	for _, col := range columns {
		qcol := quoteIdentifier(col)
		columnList = append(columnList, qcol)
		switch col {
		case "sync_id":
			selectList = append(selectList, "? as "+qcol)
		case "discovered_at":
			selectList = append(selectList, "? as "+qcol)
		default:
			selectList = append(selectList, sourceAlias+"."+qcol)
		}
	}

	args := []any{toSyncID, discoveredAt.Format("2006-01-02 15:04:05.999999999"), fromSyncID, sourceCacheKey}
	extraWhere := ""
	if len(extraWhereAndArgs) > 0 {
		var whereFn replayWhereFn
		switch fn := extraWhereAndArgs[0].(type) {
		case replayWhereFn:
			whereFn = fn
		case func(string) string:
			whereFn = fn
		default:
			return 0, fmt.Errorf("invalid source cache replay where callback")
		}
		extraWhere = whereFn(sourceAlias)
		args = append(args, extraWhereAndArgs[1:]...)
	}

	// #nosec G201 -- identifiers are internal table/column names selected from fixed row-kind mappings and PRAGMA-validated columns.
	query := fmt.Sprintf(
		`insert or replace into main.%s (%s)
select %s
from %s.%s as %s
where %s.sync_id = ? and %s.source_cache_key = ?%s`,
		tableName,
		strings.Join(columnList, ", "),
		strings.Join(selectList, ", "),
		sourceSchema,
		tableName,
		sourceAlias,
		sourceAlias,
		sourceAlias,
		extraWhere,
	)
	result, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func sourceCacheTableForRowKind(rowKind sourcecache.RowKind) (string, error) {
	switch rowKind {
	case sourcecache.RowKindResources:
		return resources.Name(), nil
	case sourcecache.RowKindEntitlements:
		return entitlements.Name(), nil
	case sourcecache.RowKindGrants:
		return grants.Name(), nil
	default:
		return "", fmt.Errorf("invalid source cache row kind: %q", rowKind)
	}
}

func sourceCacheTableHasRowColumn(ctx context.Context, c *C1File, tableName string) bool {
	if tableName != resources.Name() && tableName != entitlements.Name() && tableName != grants.Name() {
		return false
	}
	if c == nil || c.db == nil {
		return false
	}
	rows, err := c.db.QueryContext(ctx, fmt.Sprintf("select 1 from pragma_table_info('%s') where name='source_cache_key' limit 1", tableName))
	if err != nil {
		return false
	}
	defer rows.Close()
	return rows.Next()
}
