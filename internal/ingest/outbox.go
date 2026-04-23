package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; CGO_ENABLED=0 friendly

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// Outbox is the durable, single-writer queue of pending ingest batches.
// When a Send fails with a retryable error, the runner enqueues the body so
// a later sync cycle can flush it. The SQLite file lives next to the agent
// config at %ProgramData%\GST Reco\agent\outbox.db on Windows.
//
// Schema invariant: request_id is UNIQUE so the same batch can't slip in
// twice (helpful during partial enqueue failures). is_final is stored so the
// runner can reconstruct the "last batch of a run" marker when replaying
// from the outbox.
type Outbox struct {
	db *sql.DB
}

// Entry is a queued batch ready to ship.
type Entry struct {
	ID          int64
	RunID       string
	RequestID   string
	Kind        tally.IngestKind
	Company     string
	IsFinal     bool
	Body        tally.IngestRequestBody
	Attempts    int
	LastError   string
	LastAttempt time.Time
	CreatedAt   time.Time
}

// Open connects to (or creates) the outbox DB at path. WAL mode is enabled
// so a power loss mid-write can't corrupt the main DB file. busy_timeout
// gives the single reader/writer a 5s grace window if two goroutines race.
func Open(path string) (*Outbox, error) {
	// modernc.org/sqlite uses a "file:" DSN prefix; adding _pragma params
	// keeps the opening in sync with our durability expectations.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("outbox: open %s: %w", path, err)
	}
	// The agent is a single writer; a connection pool of 1 avoids
	// SQLITE_BUSY errors when two goroutines race to enqueue.
	db.SetMaxOpenConns(1)

	o := &Outbox{db: db}
	if err := o.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return o, nil
}

// Close closes the underlying DB handle. Safe to call multiple times.
func (o *Outbox) Close() error {
	if o == nil || o.db == nil {
		return nil
	}
	return o.db.Close()
}

// migrate creates the outbox table on first open. Uses IF NOT EXISTS so
// reopen is idempotent — future schema changes go in new migrate steps
// alongside a schema_version table.
func (o *Outbox) migrate(ctx context.Context) error {
	_, err := o.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS outbox (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id           TEXT    NOT NULL,
  request_id       TEXT    NOT NULL UNIQUE,
  kind             TEXT    NOT NULL,
  tally_company    TEXT    NOT NULL,
  is_final         INTEGER NOT NULL DEFAULT 0,
  body             BLOB    NOT NULL,
  attempts         INTEGER NOT NULL DEFAULT 0,
  last_attempt_at  INTEGER,
  last_error       TEXT,
  created_at       INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_outbox_run ON outbox(run_id);
CREATE INDEX IF NOT EXISTS idx_outbox_created ON outbox(created_at);
`)
	if err != nil {
		return fmt.Errorf("outbox: migrate: %w", err)
	}
	return nil
}

// Enqueue persists a batch for later delivery. Uses INSERT OR IGNORE on the
// unique request_id so a caller retrying the same body after a partial
// failure doesn't produce duplicates.
func (o *Outbox) Enqueue(ctx context.Context, body tally.IngestRequestBody) (int64, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("outbox: marshal: %w", err)
	}
	isFinal := 0
	if body.IsFinal {
		isFinal = 1
	}
	res, err := o.db.ExecContext(ctx, `
INSERT OR IGNORE INTO outbox (run_id, request_id, kind, tally_company, is_final, body)
VALUES (?, ?, ?, ?, ?, ?)
`, body.RunID, body.RequestID, string(body.Kind), body.TallyCompany, isFinal, raw)
	if err != nil {
		return 0, fmt.Errorf("outbox: insert: %w", err)
	}
	// SQLite's last_insert_rowid() is sticky on INSERT OR IGNORE — a suppressed
	// insert preserves the previous rowid. RowsAffected() is the reliable
	// "did we actually insert" signal; callers use id==0 to detect a
	// deduplicated enqueue.
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("outbox: rows affected: %w", err)
	}
	if affected == 0 {
		return 0, nil
	}
	return res.LastInsertId()
}

// Pending returns up to limit oldest entries, oldest first. Callers work the
// list FIFO so a stalled run doesn't starve newer runs' first batches —
// though in practice the entire queue belongs to at most a few runs.
func (o *Outbox) Pending(ctx context.Context, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := o.db.QueryContext(ctx, `
SELECT id, run_id, request_id, kind, tally_company, is_final, body, attempts,
       COALESCE(last_attempt_at, 0), COALESCE(last_error, ''), created_at
FROM outbox
ORDER BY id ASC
LIMIT ?
`, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: query pending: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var (
			e       Entry
			kindStr string
			isFinal int
			body    []byte
			last    int64
			created int64
		)
		if err := rows.Scan(&e.ID, &e.RunID, &e.RequestID, &kindStr, &e.Company, &isFinal, &body, &e.Attempts, &last, &e.LastError, &created); err != nil {
			return nil, fmt.Errorf("outbox: scan: %w", err)
		}
		e.Kind = tally.IngestKind(kindStr)
		e.IsFinal = isFinal != 0
		if last > 0 {
			e.LastAttempt = time.Unix(last, 0).UTC()
		}
		e.CreatedAt = time.Unix(created, 0).UTC()
		if err := json.Unmarshal(body, &e.Body); err != nil {
			return nil, fmt.Errorf("outbox: unmarshal entry %d: %w", e.ID, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Ack deletes an entry after a successful Send. Not "update status" — we
// don't need delivery history in the outbox, and deleting keeps the working
// set small (it's in the ingest log + sync runs on the server instead).
func (o *Outbox) Ack(ctx context.Context, id int64) error {
	_, err := o.db.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("outbox: ack %d: %w", id, err)
	}
	return nil
}

// MarkFailed bumps attempts and stashes the error text for later diagnosis.
// Does NOT delete — non-retryable errors are deleted via a separate Drop()
// so the caller has to be explicit about what counts as permanent failure.
func (o *Outbox) MarkFailed(ctx context.Context, id int64, errText string) error {
	_, err := o.db.ExecContext(ctx, `
UPDATE outbox
SET attempts = attempts + 1,
    last_attempt_at = unixepoch(),
    last_error = ?
WHERE id = ?
`, errText, id)
	if err != nil {
		return fmt.Errorf("outbox: mark failed %d: %w", id, err)
	}
	return nil
}

// Drop removes a permanently-failed entry (auth error, bad payload). Callers
// log the reason before dropping so post-mortems are possible.
func (o *Outbox) Drop(ctx context.Context, id int64) error {
	_, err := o.db.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("outbox: drop %d: %w", id, err)
	}
	return nil
}

// Count returns the number of pending entries. Used by `agentctl status` and
// tray UI for "7 batches waiting to send" surfacing.
func (o *Outbox) Count(ctx context.Context) (int, error) {
	var n int
	err := o.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("outbox: count: %w", err)
	}
	return n, nil
}

// ErrNotFound is returned when a targeted operation addresses a missing entry.
// Currently unused by exported methods but reserved for future Get(id) once
// the tray UI wants to inspect individual batches.
var ErrNotFound = errors.New("outbox: entry not found")
