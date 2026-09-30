// Package testdb sets up the SDK tables in a disposable local Postgres for
// the store and service DB tests. It is imported only by tests.
//
// The database is named by SDK_TEST_DATABASE_URL
// (postgres://user:pass@127.0.0.1:port/<name>_test); tests are skipped when it
// is unset. Reset drops and recreates the tables of one product schema, so
// every package using it holds Lock for its whole run.
package testdb

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

//go:embed fixture.sql
var fixtureSQL string

// ProductSchemas are the schemas the SDK is deployed against.
var ProductSchemas = []string{"public", "windoc_mastra"}

// Lock serializes test packages on the shared database: go test runs
// packages in parallel, and each Reset recreates the same schemas. Call it
// from TestMain and run the returned func after m.Run. It is a no-op when
// SDK_TEST_DATABASE_URL is unset.
func Lock() (release func()) {
	dsn := os.Getenv("SDK_TEST_DATABASE_URL")
	if dsn == "" {
		return func() {}
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		panic(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		panic(err)
	}
	const key = 7700018 // any constant shared by the SDK test packages
	if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, key); err != nil {
		panic(err)
	}
	return func() {
		conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Close()
		db.Close()
	}
}

// Open connects to the test database with the given session time zone.
func Open(t *testing.T, timeZone string) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("SDK_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SDK_TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse SDK_TEST_DATABASE_URL: %v", err)
	}
	host := u.Hostname()
	if (host != "127.0.0.1" && host != "localhost" && host != "::1") || !strings.HasSuffix(u.Path, "_test") {
		t.Fatalf("SDK_TEST_DATABASE_URL must be a local database whose name ends in _test, got %s%s", host, u.Path)
	}
	q := u.Query()
	q.Set("timezone", timeZone)
	u.RawQuery = q.Encode()
	db, err := sqlx.Connect("postgres", u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Fixture writes rows into one product schema. Messages are one second apart
// unless written with MessageAt.
type Fixture struct {
	t      *testing.T
	DB     *sqlx.DB
	Schema string
	quoted string
	Clock  time.Time
}

// Reset recreates the SDK tables in schema.
func Reset(t *testing.T, db *sqlx.DB, schema string) *Fixture {
	t.Helper()
	f := &Fixture{
		t:      t,
		DB:     db,
		Schema: schema,
		quoted: `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`,
		Clock:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	f.Exec(fixtureSQL)
	return f
}

// Exec runs query with {schema} replaced by the fixture's schema.
func (f *Fixture) Exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.DB.Exec(strings.ReplaceAll(query, "{schema}", f.quoted), args...); err != nil {
		f.t.Fatalf("exec %q: %v", query, err)
	}
}

func (f *Fixture) Thread(id, owner string) {
	f.Exec(`INSERT INTO {schema}.mastra_threads (id, "resourceId", title, "createdAt", "updatedAt")
		VALUES ($1, $2, 't', NOW(), NOW())`, id, owner)
}

// Message writes a message one second after the previous one.
func (f *Fixture) Message(id, threadID, role, content string) {
	f.Clock = f.Clock.Add(time.Second)
	f.MessageAt(id, threadID, role, content, f.Clock)
}

// MessageAt writes a message at ts, the way Mastra does: both columns, the
// legacy one as UTC wall-clock.
func (f *Fixture) MessageAt(id, threadID, role, content string, ts time.Time) {
	f.Exec(`INSERT INTO {schema}.mastra_messages (id, thread_id, content, role, type, "createdAt", "createdAtZ")
		VALUES ($1, $2, $3, $4, 'v2', $5::timestamptz AT TIME ZONE 'UTC', $5::timestamptz)`, id, threadID, content, role, ts)
}

// LegacyMessage writes a message one second after the previous one with only
// the legacy "createdAt" (UTC wall-clock) and no "createdAtZ".
func (f *Fixture) LegacyMessage(id, threadID, role, content string) {
	f.Clock = f.Clock.Add(time.Second)
	f.Exec(`INSERT INTO {schema}.mastra_messages (id, thread_id, content, role, type, "createdAt", "createdAtZ")
		VALUES ($1, $2, $3, $4, 'v2', $5::timestamptz AT TIME ZONE 'UTC', NULL)`, id, threadID, content, role, f.Clock)
}

// Run writes a stream_runs row and returns its run id.
func (f *Fixture) Run(userID, threadID, status, cleanup string) string {
	id := uuid.NewString()
	var stopAt, completedAt, terminalAt any
	switch status {
	case "stopped":
		stopAt = f.Clock
	case "completed":
		completedAt = f.Clock
	case "superseded", "failed":
		terminalAt = f.Clock
	}
	f.Exec(`INSERT INTO {schema}.stream_runs
		(run_id, user_id, thread_id, user_message_id, status, cleanup_status, stop_requested_at, completed_at, terminal_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, userID, threadID, uuid.NewString(), status, cleanup, stopAt, completedAt, terminalAt)
	return id
}

// ShareLink writes a share link of threadID taken at createdAt.
func (f *Fixture) ShareLink(id, threadID, owner string, createdAt time.Time) {
	f.Exec(`INSERT INTO {schema}.share_links (id, type, reference_id, user_id, created_at, updated_at)
		VALUES ($1, 'ai_thread', $2, $3, $4, $4)`, id, threadID, owner, createdAt)
}

// Content is a Mastra v2 message body attributed to runID.
func Content(runID string) string {
	return fmt.Sprintf(`{"format":2,"parts":[{"type":"text","text":"x"}],"metadata":{"stream_run_id":%q}}`, runID)
}
