package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/A-pen-app/ai-agent-sdk/cursor"
	"github.com/A-pen-app/ai-agent-sdk/internal/testdb"
	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/ai-agent-sdk/store"
	"github.com/A-pen-app/logging"
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

const leaked = "relation windoc_core.secret_table does not exist"

// fakeStore 只實作 finish event 用到的 ListMessages；onList 在送 finish 前被呼叫。
type fakeStore struct {
	store.Agent
	onList func(ctx context.Context)
}

func (f fakeStore) ListMessages(ctx context.Context, _, _ string, _ *cursor.Position, _ int) ([]models.MessageWithFeedback, error) {
	if f.onList != nil {
		f.onList(ctx)
	}
	return nil, ctx.Err()
}

func newTestAgent(url string, st fakeStore) *agentService {
	svc := NewAgent(st, url, "/custom/api/windoc/stream").(*agentService)
	svc.idToken = func() (string, error) { return "", errors.New("no credentials in tests") }
	return svc
}

func streamChat(ctx context.Context, svc *agentService) ([]*models.StreamEnvelope, error) {
	var events []*models.StreamEnvelope
	writer := func(e *models.StreamEnvelope) error {
		events = append(events, e)
		return nil
	}
	err := svc.StreamChat(ctx, "u1", &models.StreamRequest{ThreadID: "t1", Query: "q"}, writer)
	return events, err
}

func runUpstream(t *testing.T, handler http.HandlerFunc) ([]*models.StreamEnvelope, error) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	defer upstream.Close()
	return streamChat(context.Background(), newTestAgent(upstream.URL, fakeStore{}))
}

func eventTypes(events []*models.StreamEnvelope) string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = string(e.Event)
	}
	return strings.Join(types, ",")
}

// 失敗時 client 只收到一個 error event，finish/done 照送，呼叫端拿到 ErrClientNotified。
func assertSingleError(t *testing.T, events []*models.StreamEnvelope, err error, code, message string) {
	t.Helper()
	if !errors.Is(err, ErrClientNotified) {
		t.Fatalf("want ErrClientNotified, got %v", err)
	}
	want := strings.Join([]string{
		string(models.StreamEventStart), string(models.StreamEventError),
		string(models.StreamEventFinish), string(models.StreamEventDone),
	}, ",")
	if got := eventTypes(events); got != want {
		t.Fatalf("want events %s, got %s", want, got)
	}
	data := events[1].Data.(models.StreamErrorData)
	if data.Code != code || data.Message != message {
		t.Fatalf("unexpected error event: %#v", data)
	}
	if strings.Contains(data.Message, leaked) {
		t.Fatal("upstream error text leaked to client")
	}
}

func writeChunk(w http.ResponseWriter, chunk string) {
	fmt.Fprintf(w, "data: %s\n\n", chunk)
	w.(http.Flusher).Flush()
}

// 上游 error chunk 的原文不能出現在給 client 的 error event。
func TestUpstreamErrorChunkIsFixed(t *testing.T) {
	cases := map[string]string{
		"string": fmt.Sprintf("%q", leaked),
		"object": fmt.Sprintf(`{"name":"Error","message":%q}`, leaked),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			events, err := runUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				writeChunk(w, fmt.Sprintf(`{"type":"error","payload":{"error":%s}}`, payload))
			})
			assertSingleError(t, events, err, "UPSTREAM_ERROR", "AI 服務發生錯誤")
		})
	}
}

// error chunk 之後讀到上游關閉才送 finish（pen-gpt 在關閉前清掉殘缺訊息），
// 之後的 chunk（含重複的 error）都不轉給 client。
func TestUpstreamErrorChunkWaitsForUpstreamClose(t *testing.T) {
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		writeChunk(w, `{"type":"error","payload":{"error":"first"}}`)
		time.Sleep(100 * time.Millisecond) // pen-gpt 收尾
		writeChunk(w, `{"type":"error","payload":{"error":"second"}}`)
		writeChunk(w, `{"type":"text-delta","payload":{"text":"after"}}`)
	}))
	defer upstream.Close()

	listedAfterClose := false
	st := fakeStore{onList: func(context.Context) {
		select {
		case <-upstreamDone:
			listedAfterClose = true
		default:
		}
	}}
	events, err := streamChat(context.Background(), newTestAgent(upstream.URL, st))
	assertSingleError(t, events, err, "UPSTREAM_ERROR", "AI 服務發生錯誤")
	if !listedAfterClose {
		t.Fatal("finish was built before the upstream closed the stream")
	}
}

func TestUpstreamNonOKStatusIsFixed(t *testing.T) {
	events, err := runUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, leaked, http.StatusServiceUnavailable)
	})
	assertSingleError(t, events, err, "UPSTREAM_ERROR", "AI 服務發生錯誤")
}

func TestUpstreamUnreachable(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	upstream.Close()
	events, err := streamChat(context.Background(), newTestAgent(upstream.URL, fakeStore{}))
	assertSingleError(t, events, err, "UPSTREAM_ERROR", "AI 服務暫時無法使用，請稍後再試")
}

// testRunID 是假 pen-gpt 在 stream response header 回的 run id。
const testRunID = "3b241101-e2bb-4255-8caf-4136c566a962"

// stopRecorder 接 pen-gpt 的 /stop，只算帶對 runId 的 stop（和 pen-gpt 一樣，缺 runId 回 400）；
// stream 回應帶 x-stream-run-id。
func stopRecorder(stops *atomic.Int32, stream http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/custom/api/windoc/stop" {
			var body struct {
				ThreadID string `json:"threadId"`
				RunID    string `json:"runId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RunID != testRunID {
				http.Error(w, `{"error":"runId is required"}`, http.StatusBadRequest)
				return
			}
			stops.Add(1)
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		w.Header().Set(streamRunIDHeader, testRunID)
		stream(w, r)
	}
}

// 串流中途斷掉（這裡用超過 scanner 上限的一行）要回報失敗，不能當成正常結束，並叫 pen-gpt 停止。
func TestUpstreamReadFailureIsReported(t *testing.T) {
	var stops atomic.Int32
	events, err := runUpstream(t, stopRecorder(&stops, func(w http.ResponseWriter, r *http.Request) {
		writeChunk(w, `{"type":"text-delta","payload":{"text":"partial"}}`)
		fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("a", 2*1024*1024))
	}))
	if !errors.Is(err, ErrClientNotified) {
		t.Fatalf("want ErrClientNotified, got %v", err)
	}
	want := "start,text_delta,error,finish,done"
	if got := eventTypes(events); got != want {
		t.Fatalf("want events %s, got %s", want, got)
	}
	if stops.Load() != 1 {
		t.Fatalf("want 1 upstream stop, got %d", stops.Load())
	}
}

// 呼叫端帶 cause 取消（如逾時）是失敗：送一個 error event、回 ErrClientNotified、叫 pen-gpt 停止。
func TestCancelWithCauseIsReported(t *testing.T) {
	var stops atomic.Int32
	upstream := httptest.NewServer(stopRecorder(&stops, func(w http.ResponseWriter, r *http.Request) {
		writeChunk(w, `{"type":"text-delta","payload":{"text":"partial"}}`)
		<-r.Context().Done()
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancelCause(context.Background())
	var events []*models.StreamEnvelope
	writer := func(e *models.StreamEnvelope) error {
		events = append(events, e)
		if e.Event == models.StreamEventTextDelta {
			cancel(errors.New("idle timeout"))
		}
		return nil
	}
	err := newTestAgent(upstream.URL, fakeStore{}).StreamChat(ctx, "u1", &models.StreamRequest{ThreadID: "t1", Query: "q"}, writer)
	if !errors.Is(err, ErrClientNotified) {
		t.Fatalf("want ErrClientNotified, got %v", err)
	}
	if got := eventTypes(events); got != "start,text_delta,error,finish,done" {
		t.Fatalf("unexpected events %s", got)
	}
	if stops.Load() != 1 {
		t.Fatalf("want 1 upstream stop, got %d", stops.Load())
	}
}

// 被停止後 finish 仍查得到訊息：查詢不能沿用已取消的串流 context。
func TestStoppedStreamStillListsMessages(t *testing.T) {
	var stops atomic.Int32
	upstream := httptest.NewServer(stopRecorder(&stops, func(w http.ResponseWriter, r *http.Request) {
		writeChunk(w, `{"type":"text-delta","payload":{"text":"partial"}}`)
		<-r.Context().Done()
	}))
	defer upstream.Close()

	var listErr error
	svc := newTestAgent(upstream.URL, fakeStore{onList: func(ctx context.Context) { listErr = ctx.Err() }})
	writer := func(e *models.StreamEnvelope) error {
		if e.Event == models.StreamEventTextDelta {
			if err := svc.PauseStream(context.Background(), "t1", "u1"); err != nil {
				t.Errorf("PauseStream: %v", err)
			}
		}
		return nil
	}
	if err := svc.StreamChat(context.Background(), "u1", &models.StreamRequest{ThreadID: "t1", Query: "q"}, writer); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if listErr != nil {
		t.Fatalf("finish listed messages with a cancelled context: %v", listErr)
	}
}

// 建不出上游請求時送 INTERNAL_ERROR，一樣回 ErrClientNotified，呼叫端不會再補送。
func TestInternalErrorIsClientNotified(t *testing.T) {
	events, err := streamChat(context.Background(), newTestAgent("http://bad host", fakeStore{}))
	assertSingleError(t, events, err, "INTERNAL_ERROR", "failed to create upstream request")
}

// 被停止不是失敗：不送 error event，回傳 nil。
func TestCancelledBeforeResponseIsNotAnError(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events, err := streamChat(ctx, newTestAgent(upstream.URL, fakeStore{}))
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if got := eventTypes(events); got != "start,finish,done" {
		t.Fatalf("unexpected events %s", got)
	}
}

func TestLogSnippetKeepsValidUTF8(t *testing.T) {
	s := logSnippet([]byte(strings.Repeat("錯", upstreamLogLimit)))
	if !utf8.ValidString(s) || len(s) > upstreamLogLimit || len(s) < upstreamLogLimit-3 {
		t.Fatalf("bad snippet: len=%d valid=%v", len(s), utf8.ValidString(s))
	}
}
