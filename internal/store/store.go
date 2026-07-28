// Package store persists the control plane in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo

	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/model"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on a uniqueness violation.
var ErrConflict = errors.New("conflict")

// Store is the control-plane database.
type Store struct {
	db *sql.DB
}

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS base (
  id                  TEXT PRIMARY KEY,
  name                TEXT NOT NULL UNIQUE,
  description         TEXT NOT NULL DEFAULT '',
  vector_store        TEXT NOT NULL,
  embedder            TEXT NOT NULL,
  enable_source_sync  INTEGER NOT NULL DEFAULT 1,
  source_defaults     TEXT NOT NULL,
  retrieval           TEXT NOT NULL,
  index_revision      INTEGER NOT NULL DEFAULT 1,
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS source (
  id                  TEXT PRIMARY KEY,
  base_id             TEXT NOT NULL REFERENCES base(id) ON DELETE CASCADE,
  display_name        TEXT NOT NULL,
  type                TEXT NOT NULL,
  config              TEXT NOT NULL,
  config_hash         TEXT NOT NULL,
  synced_config_hash  TEXT NOT NULL DEFAULT '',
  user_metadata       TEXT NOT NULL DEFAULT '{}',
  state               TEXT NOT NULL,
  last_synced_at      TEXT,
  doc_count           INTEGER NOT NULL DEFAULT 0,
  chunk_count         INTEGER NOT NULL DEFAULT 0,
  last_error          TEXT NOT NULL DEFAULT '',
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL,
  UNIQUE (base_id, display_name)
);
CREATE INDEX IF NOT EXISTS source_by_base ON source(base_id);

CREATE TABLE IF NOT EXISTS task (
  id                  TEXT PRIMARY KEY,
  base_id             TEXT NOT NULL REFERENCES base(id) ON DELETE CASCADE,
  source_id           TEXT NOT NULL DEFAULT '',
  kind                TEXT NOT NULL,
  state               TEXT NOT NULL,
  requested_by        TEXT NOT NULL DEFAULT 'user',
  params              TEXT NOT NULL DEFAULT '{}',
  progress            TEXT NOT NULL DEFAULT '{}',
  counters_before     TEXT NOT NULL DEFAULT '{}',
  counters_after      TEXT NOT NULL DEFAULT '{}',
  error               TEXT NOT NULL DEFAULT '',
  created_at          TEXT NOT NULL,
  started_at          TEXT,
  finished_at         TEXT
);
CREATE INDEX IF NOT EXISTS task_by_base_state ON task(base_id, state);

-- Writes against one BuiltinKnowledge serialize behind dataOperationMu
-- upstream. Enforce the same invariant here so a bug in the queue surfaces as
-- a constraint violation instead of a deadlock.
CREATE UNIQUE INDEX IF NOT EXISTS task_one_running_per_base
  ON task(base_id) WHERE state = 'running';
`

// Open opens (and migrates) the control-plane database.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc's driver is not safe for concurrent writers on one connection.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// ---------------------------------------------------------------- helpers

// NewID returns a prefixed random identifier.
func NewID(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return prefix + "_" + fmt.Sprint(time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// HashConfig produces a stable hash of a source configuration. It is computed
// from the canonical JSON encoding, so field order in the request body cannot
// produce a spurious "needs sync".
func HashConfig(cfg model.SourceConfig, md map[string]any) string {
	payload := struct {
		Config   model.SourceConfig `json:"config"`
		Metadata map[string]any     `json:"metadata"`
	}{cfg, md}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func fromJSON(s string, v any) {
	if s == "" {
		return
	}
	_ = json.Unmarshal([]byte(s), v)
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func parseTSPtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTS(s.String)
	return &t
}

// ---------------------------------------------------------------- base

// CreateBase inserts a knowledge base.
func (s *Store) CreateBase(ctx context.Context, b *model.Base) error {
	now := time.Now()
	b.CreatedAt, b.UpdatedAt = now, now
	if b.ID == "" {
		b.ID = NewID("kb")
	}
	if b.IndexRevision == 0 {
		b.IndexRevision = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO base (id, name, description, vector_store, embedder, enable_source_sync,
		                  source_defaults, retrieval, index_revision, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.Name, b.Description, mustJSON(b.VectorStore), mustJSON(b.Embedder),
		boolToInt(b.EnableSourceSync), mustJSON(b.SourceDefaults), mustJSON(b.Retrieval),
		b.IndexRevision, ts(now), ts(now))
	if err != nil {
		return wrapUnique(err)
	}
	return nil
}

// GetBase loads one knowledge base.
func (s *Store) GetBase(ctx context.Context, id string) (*model.Base, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, description, vector_store, embedder, enable_source_sync,
		       source_defaults, retrieval, index_revision, created_at, updated_at
		FROM base WHERE id = ?`, id)
	b, err := scanBase(row)
	if err != nil {
		return nil, err
	}
	if err := s.fillBaseCounts(ctx, b); err != nil {
		return nil, err
	}
	return b, nil
}

// ListBases returns all knowledge bases with denormalized counts.
func (s *Store) ListBases(ctx context.Context) ([]*model.Base, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, description, vector_store, embedder, enable_source_sync,
		       source_defaults, retrieval, index_revision, created_at, updated_at
		FROM base ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Base
	for rows.Next() {
		b, err := scanBase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, b := range out {
		if err := s.fillBaseCounts(ctx, b); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) fillBaseCounts(ctx context.Context, b *model.Base) error {
	return s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(doc_count),0), COALESCE(SUM(chunk_count),0)
		FROM source WHERE base_id = ?`, b.ID).
		Scan(&b.SourceCount, &b.DocCount, &b.ChunkCount)
}

// UpdateBase persists an edited knowledge base.
func (s *Store) UpdateBase(ctx context.Context, b *model.Base) error {
	b.UpdatedAt = time.Now()
	res, err := s.db.ExecContext(ctx, `
		UPDATE base SET name=?, description=?, vector_store=?, embedder=?, enable_source_sync=?,
		                source_defaults=?, retrieval=?, index_revision=?, updated_at=?
		WHERE id=?`,
		b.Name, b.Description, mustJSON(b.VectorStore), mustJSON(b.Embedder),
		boolToInt(b.EnableSourceSync), mustJSON(b.SourceDefaults), mustJSON(b.Retrieval),
		b.IndexRevision, ts(b.UpdatedAt), b.ID)
	if err != nil {
		return wrapUnique(err)
	}
	return affected(res)
}

// DeleteBase removes a knowledge base and everything under it.
func (s *Store) DeleteBase(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM base WHERE id=?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

type scanner interface{ Scan(dest ...any) error }

func scanBase(r scanner) (*model.Base, error) {
	var (
		b                                     model.Base
		vs, emb, defaults, retr, created, upd string
		sync                                  int
	)
	if err := r.Scan(&b.ID, &b.Name, &b.Description, &vs, &emb, &sync,
		&defaults, &retr, &b.IndexRevision, &created, &upd); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	fromJSON(vs, &b.VectorStore)
	fromJSON(emb, &b.Embedder)
	fromJSON(defaults, &b.SourceDefaults)
	fromJSON(retr, &b.Retrieval)
	b.EnableSourceSync = sync != 0
	b.CreatedAt, b.UpdatedAt = parseTS(created), parseTS(upd)
	return &b, nil
}

// ---------------------------------------------------------------- source

// CreateSource inserts a source. The caller must have validated the config.
func (s *Store) CreateSource(ctx context.Context, src *model.Source) error {
	now := time.Now()
	src.CreatedAt, src.UpdatedAt = now, now
	if src.ID == "" {
		src.ID = NewID("src")
	}
	if src.State == "" {
		src.State = model.SourceActive
	}
	src.ConfigHash = HashConfig(src.Config, src.UserMetadata)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO source (id, base_id, display_name, type, config, config_hash,
		                    synced_config_hash, user_metadata, state, doc_count, chunk_count,
		                    last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,'',?,?,0,0,'',?,?)`,
		src.ID, src.BaseID, src.DisplayName, src.Type, mustJSON(src.Config), src.ConfigHash,
		mustJSON(src.UserMetadata), src.State, ts(now), ts(now))
	if err != nil {
		return wrapUnique(err)
	}
	return nil
}

// GetSource loads one source.
func (s *Store) GetSource(ctx context.Context, id string) (*model.Source, error) {
	row := s.db.QueryRowContext(ctx, sourceCols+` WHERE id = ?`, id)
	return scanSource(row)
}

// ListSources returns every source of a base, oldest first.
func (s *Store) ListSources(ctx context.Context, baseID string) ([]*model.Source, error) {
	rows, err := s.db.QueryContext(ctx, sourceCols+` WHERE base_id = ? ORDER BY created_at`, baseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// UpdateSourceConfig persists a configuration edit. It deliberately does not
// touch the running knowledge base: configuration and effect are decoupled, so
// the row simply becomes stale until a sync task runs.
func (s *Store) UpdateSourceConfig(ctx context.Context, src *model.Source) error {
	src.UpdatedAt = time.Now()
	src.ConfigHash = HashConfig(src.Config, src.UserMetadata)
	res, err := s.db.ExecContext(ctx, `
		UPDATE source SET display_name=?, type=?, config=?, config_hash=?, user_metadata=?,
		                  state=?, updated_at=?
		WHERE id=?`,
		src.DisplayName, src.Type, mustJSON(src.Config), src.ConfigHash,
		mustJSON(src.UserMetadata), src.State, ts(src.UpdatedAt), src.ID)
	if err != nil {
		return wrapUnique(err)
	}
	return affected(res)
}

// MarkSourceSynced records a successful ingest.
func (s *Store) MarkSourceSynced(ctx context.Context, id, hash string, c model.Counters) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE source SET synced_config_hash=?, last_synced_at=?, doc_count=?, chunk_count=?,
		                  last_error='', updated_at=? WHERE id=?`,
		hash, ts(now), c.Docs, c.Chunks, ts(now), id)
	return err
}

// MarkSourceFailed records an ingest failure without clearing the previous
// successful sync — the old vectors are still there.
func (s *Store) MarkSourceFailed(ctx context.Context, id, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE source SET last_error=?, updated_at=? WHERE id=?`, msg, ts(time.Now()), id)
	return err
}

// ResetSyncState forgets that a base's sources were ever ingested.
//
// Called at startup for bases backed by a non-persistent store: the vectors
// are provably gone, so leaving synced_config_hash set would show a green
// "synced" for a source whose content no longer exists, and would send the
// next sync down the incremental reload path, which reconciles against
// document info held in that same empty store.
func (s *Store) ResetSyncState(ctx context.Context, baseID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE source SET synced_config_hash='', chunk_count=0, doc_count=0, last_synced_at=NULL,
		                  updated_at=?
		WHERE base_id=? AND synced_config_hash<>''`, ts(time.Now()), baseID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteSource removes the row.
func (s *Store) DeleteSource(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM source WHERE id=?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

const sourceCols = `
SELECT id, base_id, display_name, type, config, config_hash, synced_config_hash,
       user_metadata, state, last_synced_at, doc_count, chunk_count, last_error,
       created_at, updated_at
FROM source`

func scanSource(r scanner) (*model.Source, error) {
	var (
		src                   model.Source
		cfg, md, created, upd string
		lastSync              sql.NullString
	)
	if err := r.Scan(&src.ID, &src.BaseID, &src.DisplayName, &src.Type, &cfg, &src.ConfigHash,
		&src.SyncedConfigHash, &md, &src.State, &lastSync, &src.DocCount, &src.ChunkCount,
		&src.LastError, &created, &upd); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	fromJSON(cfg, &src.Config)
	fromJSON(md, &src.UserMetadata)
	src.LastSyncedAt = parseTSPtr(lastSync)
	src.CreatedAt, src.UpdatedAt = parseTS(created), parseTS(upd)
	return &src, nil
}

// ---------------------------------------------------------------- task

// CreateTask enqueues a task.
func (s *Store) CreateTask(ctx context.Context, t *model.Task) error {
	now := time.Now()
	t.CreatedAt = now
	if t.ID == "" {
		t.ID = NewID("task")
	}
	if t.State == "" {
		t.State = model.TaskQueued
	}
	if t.RequestedBy == "" {
		t.RequestedBy = "user"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO task (id, base_id, source_id, kind, state, requested_by, params,
		                  progress, counters_before, counters_after, error, created_at)
		VALUES (?,?,?,?,?,?,?,'{}','{}','{}','',?)`,
		t.ID, t.BaseID, t.SourceID, t.Kind, t.State, t.RequestedBy, mustJSON(t.Params), ts(now))
	return err
}

// GetTask loads one task.
func (s *Store) GetTask(ctx context.Context, id string) (*model.Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, taskCols+` WHERE id = ?`, id))
}

// ListTasks returns the most recent tasks of a base.
func (s *Store) ListTasks(ctx context.Context, baseID string, limit int) ([]*model.Task, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, taskCols+`
		WHERE base_id = ? ORDER BY created_at DESC LIMIT ?`, baseID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PendingTasks returns queued tasks across all bases, oldest first. Used to
// re-arm the queue after a restart.
func (s *Store) PendingTasks(ctx context.Context) ([]*model.Task, error) {
	rows, err := s.db.QueryContext(ctx, taskCols+`
		WHERE state IN ('queued','running') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StartTask marks a task running and records the starting counters.
func (s *Store) StartTask(ctx context.Context, id string, before model.Counters) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE task SET state='running', started_at=?, counters_before=? WHERE id=?`,
		ts(time.Now()), mustJSON(before), id)
	return err
}

// SaveProgress stores the latest progress snapshot.
func (s *Store) SaveProgress(ctx context.Context, id string, p model.Progress) error {
	_, err := s.db.ExecContext(ctx, `UPDATE task SET progress=? WHERE id=?`, mustJSON(p), id)
	return err
}

// FinishTask marks a task terminal.
func (s *Store) FinishTask(ctx context.Context, id string, state model.TaskState,
	after model.Counters, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE task SET state=?, finished_at=?, counters_after=?, error=? WHERE id=?`,
		state, ts(time.Now()), mustJSON(after), errMsg, id)
	return err
}

const taskCols = `
SELECT id, base_id, source_id, kind, state, requested_by, params, progress,
       counters_before, counters_after, error, created_at, started_at, finished_at
FROM task`

func scanTask(r scanner) (*model.Task, error) {
	var (
		t                             model.Task
		params, prog, cb, ca, created string
		started, finished             sql.NullString
	)
	if err := r.Scan(&t.ID, &t.BaseID, &t.SourceID, &t.Kind, &t.State, &t.RequestedBy,
		&params, &prog, &cb, &ca, &t.Error, &created, &started, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	fromJSON(params, &t.Params)
	fromJSON(prog, &t.Progress)
	fromJSON(cb, &t.CountersBefore)
	fromJSON(ca, &t.CountersAfter)
	t.CreatedAt = parseTS(created)
	t.StartedAt, t.FinishedAt = parseTSPtr(started), parseTSPtr(finished)
	return &t, nil
}

// ---------------------------------------------------------------- misc

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func wrapUnique(err error) error {
	if err == nil {
		return nil
	}
	if msg := err.Error(); containsAny(msg, "UNIQUE constraint failed", "constraint failed: UNIQUE") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) <= len(s) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
