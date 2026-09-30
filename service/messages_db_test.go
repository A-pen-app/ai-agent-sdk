package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/A-pen-app/ai-agent-sdk/cursor"
	"github.com/A-pen-app/ai-agent-sdk/internal/testdb"
	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/ai-agent-sdk/store"
	"github.com/jmoiron/sqlx"
)

var sessionTimeZones = []string{"UTC", "Asia/Taipei"}

const owner, threadID, linkID = "owner", "thread-1", "link-1"

// seedConversation writes a thread whose visible messages, oldest first, are
// returned. It mixes messages sharing one timestamp, a stopped run whose
// assistant messages are hidden, and a legacy row without "createdAtZ".
func seedConversation(f *testdb.Fixture) []string {
	f.Thread(threadID, owner)
	var visible []string
	add := func(id, role, content string) {
		f.Message(id, threadID, role, content)
		visible = append(visible, id)
	}

	add("m01", "user", "{}")
	add("m02", "assistant", "{}")

	// Four messages in the same microsecond: only the id orders them.
	f.Clock = f.Clock.Add(time.Second)
	for _, c := range []struct{ id, role string }{{"s-a", "user"}, {"s-b", "assistant"}, {"s-c", "user"}, {"s-d", "assistant"}} {
		f.MessageAt(c.id, threadID, c.role, "{}", f.Clock)
		visible = append(visible, c.id)
	}

	stopped := f.Run(owner, threadID, "stopped", "pending")
	add("m03", "user", testdb.Content(stopped))
	f.Message("m04-hidden", threadID, "assistant", testdb.Content(stopped))
	f.Message("m05-hidden", threadID, "assistant", testdb.Content(stopped))

	f.LegacyMessage("m06-legacy", threadID, "user", "{}")
	visible = append(visible, "m06-legacy")

	add("m07", "assistant", "{}")
	add("m08", "user", "{}")
	add("m09", "assistant", "{}")

	f.ShareLink(linkID, threadID, owner, f.Clock.Add(time.Minute))
	return visible
}

func services(db *sqlx.DB, schema string) (Agent, Share) {
	s := store.NewAgent(db, schema)
	return NewAgent(s, "http://127.0.0.1:0"), NewShare(s, "http://127.0.0.1:0", nil)
}

func messageIDs[T any](data []T, id func(T) string) []string {
	out := make([]string, len(data))
	for i, d := range data {
		out[i] = id(d)
	}
	return out
}

func decodeNext(t *testing.T, next *string) *cursor.Position {
	t.Helper()
	p, err := cursor.Decode(*next)
	if err != nil || p == nil {
		t.Fatalf("next %q does not decode: %v", *next, err)
	}
	return p
}

// walkMessages follows next from the newest page back and returns every
// message oldest first. Each page's next must name the oldest message on it:
// the last row of the store's newest-first page, before the service reverses
// it for display.
func walkMessages(t *testing.T, agent Agent, count int, onPage func(next string)) []string {
	t.Helper()
	var pages [][]string
	token := ""
	for range 20 {
		resp, err := agent.ListMessages(context.Background(), threadID, owner, token, count)
		if err != nil {
			t.Fatalf("ListMessages(%q): %v", token, err)
		}
		ids := messageIDs(resp.Data, func(m models.MessageResponse) string { return m.ID })
		pages = append(pages, ids)
		if resp.Next == nil {
			var all []string
			for _, p := range slices.Backward(pages) {
				all = append(all, p...)
			}
			return all
		}
		if got := decodeNext(t, resp.Next).ID; got != ids[0] {
			t.Fatalf("next names %s, want the oldest message on the page %s (page %v)", got, ids[0], ids)
		}
		token = *resp.Next
		if onPage != nil {
			onPage(token)
		}
	}
	t.Fatal("ListMessages did not reach the last page")
	return nil
}

func walkShared(t *testing.T, share Share, count int, onPage func(next string)) []string {
	t.Helper()
	var all []string
	token := ""
	for range 20 {
		resp, err := share.ListSharedMessages(context.Background(), linkID, token, count)
		if err != nil {
			t.Fatalf("ListSharedMessages(%q): %v", token, err)
		}
		ids := messageIDs(resp.Data, func(m models.SharedMessageResponse) string { return m.ID })
		all = append(all, ids...)
		if resp.Next == nil {
			return all
		}
		if got := decodeNext(t, resp.Next).ID; got != ids[len(ids)-1] {
			t.Fatalf("next names %s, want the newest message on the page %s (page %v)", got, ids[len(ids)-1], ids)
		}
		token = *resp.Next
		if onPage != nil {
			onPage(token)
		}
	}
	t.Fatal("ListSharedMessages did not reach the last page")
	return nil
}

func TestMessagePagesAreContinuous(t *testing.T) {
	for _, tz := range sessionTimeZones {
		db := testdb.Open(t, tz)
		for _, schema := range testdb.ProductSchemas {
			t.Run(tz+"/"+schema, func(t *testing.T) {
				f := testdb.Reset(t, db, schema)
				visible := seedConversation(f)
				agent, share := services(db, schema)
				for _, count := range []int{1, 2, 3, 100} {
					if got := walkMessages(t, agent, count, nil); !slices.Equal(got, visible) {
						t.Errorf("count %d ListMessages:\n got  %v\n want %v", count, got, visible)
					}
					if got := walkShared(t, share, count, nil); !slices.Equal(got, visible) {
						t.Errorf("count %d ListSharedMessages:\n got  %v\n want %v", count, got, visible)
					}
				}
			})
		}
	}
}

// The next page does not depend on the message the cursor was taken from:
// pen-gpt's cleanup may delete it between two requests.
func TestPagesSurviveDeletedCursorMessage(t *testing.T) {
	for _, tz := range sessionTimeZones {
		db := testdb.Open(t, tz)
		for _, schema := range testdb.ProductSchemas {
			t.Run(tz+"/"+schema, func(t *testing.T) {
				f := testdb.Reset(t, db, schema)
				visible := seedConversation(f)
				agent, share := services(db, schema)
				var deleted []string
				deleteCursorRow := func(next string) {
					p, _ := cursor.Decode(next)
					f.Exec(`DELETE FROM {schema}.mastra_messages WHERE id = $1`, p.ID)
					deleted = append(deleted, p.ID)
				}

				// Each cursor row is deleted after its page was read, so every
				// visible message is still returned exactly once.
				if got := walkMessages(t, agent, 2, deleteCursorRow); !slices.Equal(got, visible) {
					t.Errorf("ListMessages:\n got  %v\n want %v (deleted %v)", got, visible, deleted)
				}

				f = testdb.Reset(t, db, schema)
				visible = seedConversation(f)
				deleted = nil
				if got := walkShared(t, share, 2, deleteCursorRow); !slices.Equal(got, visible) {
					t.Errorf("ListSharedMessages:\n got  %v\n want %v (deleted %v)", got, visible, deleted)
				}
			})
		}
	}
}

// The same page yields the same token whatever the session time zone.
func TestNextTokenIsTheSameInEveryTimeZone(t *testing.T) {
	for _, schema := range testdb.ProductSchemas {
		t.Run(schema, func(t *testing.T) {
			var tokens, sharedTokens []string
			for _, tz := range sessionTimeZones {
				db := testdb.Open(t, tz)
				seedConversation(testdb.Reset(t, db, schema))
				agent, share := services(db, schema)
				var page []string
				walkMessages(t, agent, 2, func(next string) { page = append(page, next) })
				tokens = append(tokens, page...)
				page = nil
				walkShared(t, share, 2, func(next string) { page = append(page, next) })
				sharedTokens = append(sharedTokens, page...)
			}
			half := len(tokens) / 2
			if !slices.Equal(tokens[:half], tokens[half:]) {
				t.Errorf("ListMessages tokens differ between UTC and Asia/Taipei:\n %v\n %v", tokens[:half], tokens[half:])
			}
			half = len(sharedTokens) / 2
			if !slices.Equal(sharedTokens[:half], sharedTokens[half:]) {
				t.Errorf("ListSharedMessages tokens differ between UTC and Asia/Taipei:\n %v\n %v", sharedTokens[:half], sharedTokens[half:])
			}
		})
	}
}

func TestPreV1CursorIsInvalid(t *testing.T) {
	db := testdb.Open(t, "UTC")
	for _, schema := range testdb.ProductSchemas {
		t.Run(schema, func(t *testing.T) {
			seedConversation(testdb.Reset(t, db, schema))
			agent, share := services(db, schema)
			// Before v0.0.18 next was the id of the page's last message.
			const bareID = "m07"
			if _, err := agent.ListMessages(context.Background(), threadID, owner, bareID, 2); !errors.Is(err, ErrInvalidCursor) {
				t.Errorf("ListMessages(bare id) err = %v, want ErrInvalidCursor", err)
			}
			if _, err := share.ListSharedMessages(context.Background(), linkID, bareID, 2); !errors.Is(err, ErrInvalidCursor) {
				t.Errorf("ListSharedMessages(bare id) err = %v, want ErrInvalidCursor", err)
			}
			if !errors.Is(ErrInvalidCursor, cursor.ErrInvalid) {
				t.Error("ErrInvalidCursor is not cursor.ErrInvalid")
			}
		})
	}
}
