package store

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/A-pen-app/ai-agent-sdk/internal/testdb"
	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/logging"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func TestMain(m *testing.M) {
	if err := logging.Initialize(nil); err != nil {
		panic(err)
	}
	release := testdb.Lock()
	code := m.Run()
	release()
	os.Exit(code)
}

// fixture is a testdb.Fixture with a store bound to the same schema.
type fixture struct {
	*testdb.Fixture
	store Agent
}

func openTestDB(t *testing.T) *sqlx.DB { return testdb.Open(t, "UTC") }

var productSchemas = testdb.ProductSchemas

func newFixture(t *testing.T, db *sqlx.DB, schemaName string) *fixture {
	return &fixture{Fixture: testdb.Reset(t, db, schemaName), store: NewAgent(db, schemaName)}
}

var content = testdb.Content

func listIDs(t *testing.T, f *fixture, threadID string, count int) []string {
	t.Helper()
	rows, err := f.store.ListMessages(context.Background(), threadID, "owner", nil, count)
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
	rows, err := f.store.ListSharedMessages(context.Background(), threadID, f.Clock.Add(time.Hour), nil, count)
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
			f.Thread(thread, owner)

			var visible []string
			show := func(id, role, body string) {
				f.Message(id, thread, role, body)
				visible = append(visible, id)
			}
			hide := func(id, role, body string) { f.Message(id, thread, role, body) }

			// Every run that did not complete normally hides its assistant
			// messages whatever the cleanup progress; its user message stays.
			for _, c := range []struct{ status, cleanup string }{
				{"stopped", "pending"}, {"stopped", "done"}, {"stopped", "failed"},
				{"superseded", "pending"}, {"superseded", "done"}, {"superseded", "failed"},
				{"failed", "pending"}, {"failed", "done"}, {"failed", "failed"},
			} {
				run := f.Run(owner, thread, c.status, c.cleanup)
				show("user-"+c.status+"-"+c.cleanup, "user", content(run))
				hide("asst-"+c.status+"-"+c.cleanup, "assistant", content(run))
			}

			// Shown: runs without a marker, and anything the filter cannot
			// attribute to one of the owner's ended runs in this thread.
			completed := f.Run(owner, thread, "completed", "not_needed")
			show("asst-completed", "assistant", content(completed))
			failedNoMarker := f.Run(owner, thread, "failed", "not_needed")
			show("asst-failed-not-needed", "assistant", content(failedNoMarker))
			running := f.Run(owner, thread, "running", "not_needed")
			show("asst-running", "assistant", content(running))
			show("asst-unknown-run", "assistant", content(uuid.NewString()))
			show("asst-no-metadata", "assistant", `{"format":2,"parts":[]}`)
			show("asst-bad-json", "assistant", `{"format":2,`)
			show("asst-not-uuid", "assistant", `{"metadata":{"stream_run_id":"not-a-uuid"}}`)
			show("asst-nested-id", "assistant", `{"metadata":{"stream_run_id":{"a":1}}}`)
			show("asst-array-content", "assistant", `[1,2]`)
			otherThread := f.Run(owner, "thread-2", "stopped", "pending")
			show("asst-other-thread-run", "assistant", content(otherThread))
			otherUser := f.Run("someone-else", thread, "stopped", "pending")
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

// Each user message of an ended run carries the run's status; nothing else
// carries one.
func TestEndedRunUserMessageCarriesItsStatus(t *testing.T) {
	db := openTestDB(t)
	for _, schemaName := range productSchemas {
		t.Run(schemaName, func(t *testing.T) {
			f := newFixture(t, db, schemaName)
			const owner, thread = "owner", "thread-1"
			f.Thread(thread, owner)
			want := map[string]string{}
			for _, status := range []string{"stopped", "superseded", "failed"} {
				run := f.Run(owner, thread, status, "pending")
				f.Message("user-"+status, thread, "user", content(run))
				want["user-"+status] = status
			}
			completed := f.Run(owner, thread, "completed", "not_needed")
			f.Message("user-completed", thread, "user", content(completed))
			f.Message("asst-completed", thread, "assistant", content(completed))
			failedNoMarker := f.Run(owner, thread, "failed", "not_needed")
			f.Message("user-failed-not-needed", thread, "user", content(failedNoMarker))
			f.Message("user-no-run", thread, "user", "{}")

			check := func(name string, rows []models.MessageWithFeedback) {
				t.Helper()
				if len(rows) != 7 {
					t.Fatalf("%s returned %d rows, want 7", name, len(rows))
				}
				for _, r := range rows {
					got := ""
					if r.EndedStatus != nil {
						got = *r.EndedStatus
					}
					if got != want[r.ID] {
						t.Errorf("%s %s ended_status = %q, want %q", name, r.ID, got, want[r.ID])
					}
				}
			}
			rows, err := f.store.ListMessages(context.Background(), thread, owner, nil, 100)
			if err != nil {
				t.Fatal(err)
			}
			check("ListMessages", rows)
			rows, err = f.store.ListSharedMessages(context.Background(), thread, f.Clock.Add(time.Hour), nil, 100)
			if err != nil {
				t.Fatal(err)
			}
			check("ListSharedMessages", rows)
		})
	}
}

// ListMessages lists only the caller's own thread that is not deleted.
func TestListMessagesIsScopedToTheOwner(t *testing.T) {
	db := openTestDB(t)
	for _, schemaName := range productSchemas {
		t.Run(schemaName, func(t *testing.T) {
			f := newFixture(t, db, schemaName)
			f.Thread("thread-1", "owner")
			f.Message("m1", "thread-1", "user", "{}")
			f.Thread("deleted", "owner")
			f.Message("d1", "deleted", "user", "{}")
			f.Exec(`UPDATE {schema}.mastra_threads SET "deletedAt" = NOW() WHERE id = 'deleted'`)

			for _, c := range []struct{ thread, user string }{
				{"thread-1", "someone-else"},
				{"deleted", "owner"},
			} {
				rows, err := f.store.ListMessages(context.Background(), c.thread, c.user, nil, 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 0 {
					t.Errorf("ListMessages(%s, %s) = %d rows, want none", c.thread, c.user, len(rows))
				}
			}
			if got := listIDs(t, f, "thread-1", 10); !slices.Equal(got, []string{"m1"}) {
				t.Errorf("ListMessages(owner) = %v", got)
			}
		})
	}
}

func TestMessageWithoutThreadRowIsShownOnlyToShareReaders(t *testing.T) {
	db := openTestDB(t)
	for _, schemaName := range productSchemas {
		t.Run(schemaName, func(t *testing.T) {
			f := newFixture(t, db, schemaName)
			// No mastra_threads row: nobody owns it, so ListMessages shows
			// nothing, and the owner's runs are unknown, so nothing is hidden.
			run := f.Run("owner", "orphan", "stopped", "pending")
			f.Message("orphan-asst", "orphan", "assistant", content(run))

			if got := listIDs(t, f, "orphan", 10); len(got) != 0 {
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
			f.Thread(thread, owner)
			stopped := f.Run(owner, thread, "stopped", "pending")
			f.Message("u1", thread, "user", "{}")
			f.Message("u2", thread, "user", content(stopped))
			f.Message("a2-hidden", thread, "assistant", content(stopped))
			f.Message("a2b-hidden", thread, "assistant", content(stopped))
			f.Message("u3", thread, "user", "{}")
			f.Message("a3", thread, "assistant", "{}")

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
			f.Thread("thread-1", "owner")
			f.Message("a1", "thread-1", "assistant", content(uuid.NewString()))
			f.Exec(`DROP TABLE {schema}.stream_runs`)

			if _, err := f.store.ListMessages(context.Background(), "thread-1", "owner", nil, 10); err == nil {
				t.Error("ListMessages returned no error without stream_runs")
			}
			if _, err := f.store.ListSharedMessages(context.Background(), "thread-1", f.Clock.Add(time.Hour), nil, 10); err == nil {
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
	pub.Thread("thread-1", "owner")
	pub.Message("public-msg", "thread-1", "user", "{}")
	win.Thread("thread-1", "owner")
	win.Message("windoc-msg", "thread-1", "user", "{}")

	for _, c := range []struct {
		name string
		f    *fixture
		want string
	}{{"public", pub, "public-msg"}, {"windoc_mastra", win, "windoc-msg"}} {
		if got := listIDs(t, c.f, "thread-1", 10); !slices.Equal(got, []string{c.want}) {
			t.Errorf("%s NewAgent ListMessages = %v", c.name, got)
		}
		share := NewShare(db, c.name)
		rows, err := share.ListSharedMessages(context.Background(), "thread-1", c.f.Clock.Add(time.Hour), nil, 10)
		if err != nil {
			t.Fatalf("%s NewShare ListSharedMessages: %v", c.name, err)
		}
		if len(rows) != 1 || rows[0].ID != c.want {
			t.Errorf("%s NewShare ListSharedMessages = %+v", c.name, rows)
		}
	}
}
