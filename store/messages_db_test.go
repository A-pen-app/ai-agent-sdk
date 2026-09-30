package store

import (
	"context"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/A-pen-app/logging"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// DB tests run against a disposable local Postgres named by
// SDK_TEST_DATABASE_URL (postgres://user:pass@127.0.0.1:port/<name>_test);
// they drop and recreate the SDK tables in both product schemas.

func TestMain(m *testing.M) {
	if err := logging.Initialize(nil); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

//go:embed testdata/fixture.sql
var fixtureSQL string

var productSchemas = []string{"public", "windoc_mastra"}

func openTestDB(t *testing.T) *sqlx.DB {
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
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fixture resets one product schema and returns a store bound to it.
type fixture struct {
	t      *testing.T
	db     *sqlx.DB
	schema schema
	store  Agent
	clock  time.Time
}

func newFixture(t *testing.T, db *sqlx.DB, schemaName string) *fixture {
	t.Helper()
	s := newSchema(schemaName)
	if _, err := db.Exec(s.sql(fixtureSQL)); err != nil {
		t.Fatalf("load fixture into %s: %v", schemaName, err)
	}
	return &fixture{
		t:      t,
		db:     db,
		schema: s,
		store:  NewAgent(db, schemaName),
		clock:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.schema.sql(query), args...); err != nil {
		f.t.Fatalf("exec %q: %v", query, err)
	}
}

func (f *fixture) thread(id, owner string) {
	f.exec(`INSERT INTO {schema}.mastra_threads (id, "resourceId", title, "createdAt", "updatedAt")
		VALUES ($1, $2, 't', NOW(), NOW())`, id, owner)
}

// message inserts a message one second after the previous one.
func (f *fixture) message(id, threadID, role, content string) {
	f.clock = f.clock.Add(time.Second)
	f.exec(`INSERT INTO {schema}.mastra_messages (id, thread_id, content, role, type, "createdAt", "createdAtZ")
		VALUES ($1, $2, $3, $4, 'v2', $5::timestamptz AT TIME ZONE 'UTC', $5::timestamptz)`, id, threadID, content, role, f.clock)
}

// run inserts a stream_runs row in a terminal or active state.
func (f *fixture) run(userID, threadID, status, cleanup string) string {
	id := uuid.NewString()
	var stopAt, completedAt, terminalAt any
	switch status {
	case "stopped":
		stopAt = f.clock
	case "completed":
		completedAt = f.clock
	case "superseded", "failed":
		terminalAt = f.clock
	}
	f.exec(`INSERT INTO {schema}.stream_runs
		(run_id, user_id, thread_id, user_message_id, status, cleanup_status, stop_requested_at, completed_at, terminal_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, userID, threadID, uuid.NewString(), status, cleanup, stopAt, completedAt, terminalAt)
	return id
}

func content(runID string) string {
	return fmt.Sprintf(`{"format":2,"parts":[{"type":"text","text":"x"}],"metadata":{"stream_run_id":%q}}`, runID)
}

func listIDs(t *testing.T, f *fixture, threadID string, count int) []string {
	t.Helper()
	rows, err := f.store.ListMessages(context.Background(), threadID, "viewer-is-owner", "", count)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func sharedIDs(t *testing.T, f *fixture, threadID string, count int) []string {
	t.Helper()
	rows, err := f.store.ListSharedMessages(context.Background(), threadID, f.clock.Add(time.Hour), "", count)
	if err != nil {
		t.Fatalf("ListSharedMessages: %v", err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func reversed(s []string) []string {
	out := slices.Clone(s)
	slices.Reverse(out)
	return out
}

func TestEndedRunMessagesAreHidden(t *testing.T) {
	db := openTestDB(t)
	for _, schemaName := range productSchemas {
		t.Run(schemaName, func(t *testing.T) {
			f := newFixture(t, db, schemaName)
			const owner, thread = "owner", "thread-1"
			f.thread(thread, owner)

			var visible []string
			show := func(id, role, body string) {
				f.message(id, thread, role, body)
				visible = append(visible, id)
			}
			hide := func(id, role, body string) { f.message(id, thread, role, body) }

			// Every run that did not complete normally hides its assistant
			// messages whatever the cleanup progress; its user message stays.
			for _, c := range []struct{ status, cleanup string }{
				{"stopped", "pending"}, {"stopped", "done"}, {"stopped", "failed"},
				{"superseded", "pending"}, {"superseded", "done"}, {"superseded", "failed"},
				{"failed", "pending"}, {"failed", "done"}, {"failed", "failed"},
			} {
				run := f.run(owner, thread, c.status, c.cleanup)
				show("user-"+c.status+"-"+c.cleanup, "user", content(run))
				hide("asst-"+c.status+"-"+c.cleanup, "assistant", content(run))
			}

			// Shown: runs without a marker, and anything the filter cannot
			// attribute to one of the owner's ended runs in this thread.
			completed := f.run(owner, thread, "completed", "not_needed")
			show("asst-completed", "assistant", content(completed))
			failedNoMarker := f.run(owner, thread, "failed", "not_needed")
			show("asst-failed-not-needed", "assistant", content(failedNoMarker))
			running := f.run(owner, thread, "running", "not_needed")
			show("asst-running", "assistant", content(running))
			show("asst-unknown-run", "assistant", content(uuid.NewString()))
			show("asst-no-metadata", "assistant", `{"format":2,"parts":[]}`)
			show("asst-bad-json", "assistant", `{"format":2,`)
			show("asst-not-uuid", "assistant", `{"metadata":{"stream_run_id":"not-a-uuid"}}`)
			show("asst-nested-id", "assistant", `{"metadata":{"stream_run_id":{"a":1}}}`)
			show("asst-array-content", "assistant", `[1,2]`)
			otherThread := f.run(owner, "thread-2", "stopped", "pending")
			show("asst-other-thread-run", "assistant", content(otherThread))
			otherUser := f.run("someone-else", thread, "stopped", "pending")
			show("asst-other-user-run", "assistant", content(otherUser))

			if got, want := listIDs(t, f, thread, 100), reversed(visible); !slices.Equal(got, want) {
				t.Errorf("ListMessages:\n got  %v\n want %v", got, want)
			}
			if got := sharedIDs(t, f, thread, 100); !slices.Equal(got, visible) {
				t.Errorf("ListSharedMessages:\n got  %v\n want %v", got, visible)
			}
		})
	}
}

func TestMessageWithoutThreadRowIsShown(t *testing.T) {
	db := openTestDB(t)
	for _, schemaName := range productSchemas {
		t.Run(schemaName, func(t *testing.T) {
			f := newFixture(t, db, schemaName)
			// No mastra_threads row: the owner is unknown, so nothing is hidden.
			run := f.run("owner", "orphan", "stopped", "pending")
			f.message("orphan-asst", "orphan", "assistant", content(run))

			if got := listIDs(t, f, "orphan", 10); !slices.Equal(got, []string{"orphan-asst"}) {
				t.Errorf("ListMessages = %v", got)
			}
			if got := sharedIDs(t, f, "orphan", 10); !slices.Equal(got, []string{"orphan-asst"}) {
				t.Errorf("ListSharedMessages = %v", got)
			}
		})
	}
}

func TestHiddenMessagesDoNotShrinkThePage(t *testing.T) {
	db := openTestDB(t)
	for _, schemaName := range productSchemas {
		t.Run(schemaName, func(t *testing.T) {
			f := newFixture(t, db, schemaName)
			const owner, thread = "owner", "thread-1"
			f.thread(thread, owner)
			stopped := f.run(owner, thread, "stopped", "pending")
			f.message("u1", thread, "user", "{}")
			f.message("u2", thread, "user", content(stopped))
			f.message("a2-hidden", thread, "assistant", content(stopped))
			f.message("a2b-hidden", thread, "assistant", content(stopped))
			f.message("u3", thread, "user", "{}")
			f.message("a3", thread, "assistant", "{}")

			// count+1 visible rows: the filter runs before LIMIT.
			if got, want := listIDs(t, f, thread, 3), []string{"a3", "u3", "u2", "u1"}; !slices.Equal(got, want) {
				t.Errorf("ListMessages = %v, want %v", got, want)
			}
			if got, want := sharedIDs(t, f, thread, 3), []string{"u1", "u2", "u3", "a3"}; !slices.Equal(got, want) {
				t.Errorf("ListSharedMessages = %v, want %v", got, want)
			}
		})
	}
}

func TestRunLookupFailureIsAnError(t *testing.T) {
	db := openTestDB(t)
	for _, schemaName := range productSchemas {
		t.Run(schemaName, func(t *testing.T) {
			f := newFixture(t, db, schemaName)
			f.thread("thread-1", "owner")
			f.message("a1", "thread-1", "assistant", content(uuid.NewString()))
			f.exec(`DROP TABLE {schema}.stream_runs`)

			if _, err := f.store.ListMessages(context.Background(), "thread-1", "owner", "", 10); err == nil {
				t.Error("ListMessages returned no error without stream_runs")
			}
			if _, err := f.store.ListSharedMessages(context.Background(), "thread-1", f.clock.Add(time.Hour), "", 10); err == nil {
				t.Error("ListSharedMessages returned no error without stream_runs")
			}
		})
	}
}

// The schema passed to NewAgent/NewShare is the one every query reads: rows
// in the other product's schema are invisible.
func TestStoreReadsOnlyItsSchema(t *testing.T) {
	db := openTestDB(t)
	pub := newFixture(t, db, "public")
	win := newFixture(t, db, "windoc_mastra")
	pub.thread("thread-1", "owner")
	pub.message("public-msg", "thread-1", "user", "{}")
	win.thread("thread-1", "owner")
	win.message("windoc-msg", "thread-1", "user", "{}")

	for _, c := range []struct {
		name string
		f    *fixture
		want string
	}{{"public", pub, "public-msg"}, {"windoc_mastra", win, "windoc-msg"}} {
		if got := listIDs(t, c.f, "thread-1", 10); !slices.Equal(got, []string{c.want}) {
			t.Errorf("%s NewAgent ListMessages = %v", c.name, got)
		}
		share := NewShare(db, c.name)
		rows, err := share.ListSharedMessages(context.Background(), "thread-1", c.f.clock.Add(time.Hour), "", 10)
		if err != nil {
			t.Fatalf("%s NewShare ListSharedMessages: %v", c.name, err)
		}
		if len(rows) != 1 || rows[0].ID != c.want {
			t.Errorf("%s NewShare ListSharedMessages = %+v", c.name, rows)
		}
	}
}
