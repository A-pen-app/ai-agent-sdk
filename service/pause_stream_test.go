package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/A-pen-app/ai-agent-sdk/models"
	e "github.com/A-pen-app/errors"
)

// runStore 是 PauseStream 查 stream_runs 用的假 store。
type runStore struct {
	fakeStore
	runID   string
	findErr error
	finds   *int
}

func (r runStore) FindRunningRunID(context.Context, string, string) (string, error) {
	if r.finds != nil {
		*r.finds++
	}
	return r.runID, r.findErr
}

// stopServer 是只回 /stop 的假 pen-gpt，記下每次 stop 帶的 runId。
type stopServer struct {
	mu     sync.Mutex
	runIDs []string
	status int
	body   string
}

func (s *stopServer) handler(stream http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/api/windoc/stop" {
			stream(w, r)
			return
		}
		var body struct {
			RunID string `json:"runId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.runIDs = append(s.runIDs, body.RunID)
		s.mu.Unlock()
		if s.status != 0 {
			http.Error(w, "stop failed", s.status)
			return
		}
		w.Write([]byte(s.body))
	}
}

func (s *stopServer) stops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.runIDs...)
}

func pauseAgent(t *testing.T, stop *stopServer, st runStore, stream http.HandlerFunc) *agentService {
	t.Helper()
	upstream := httptest.NewServer(stop.handler(stream))
	t.Cleanup(upstream.Close)
	svc := NewAgent(st, upstream.URL, "/custom/api/windoc/stream").(*agentService)
	svc.idToken = func() (string, error) { return "", errors.New("no credentials in tests") }
	return svc
}

// 另一台 BFF 串流的 run：本機沒有，就用 stream_runs 裡這個 thread 正在跑的那一輪。
func TestPauseWithoutLocalStreamStopsTheRunningRun(t *testing.T) {
	stop := &stopServer{body: `{"ok":true}`}
	svc := pauseAgent(t, stop, runStore{runID: "run-from-db"}, http.NotFound)
	if err := svc.PauseStream(context.Background(), "t1", "u1"); err != nil {
		t.Fatalf("PauseStream: %v", err)
	}
	if got := stop.stops(); len(got) != 1 || got[0] != "run-from-db" {
		t.Fatalf("want one stop for run-from-db, got %v", got)
	}
}

// 找不到正在跑的 run：回 not found，不送只帶 thread 的 stop。
func TestPauseWithNoRunningRunIsNotFound(t *testing.T) {
	stop := &stopServer{body: `{"ok":true}`}
	svc := pauseAgent(t, stop, runStore{}, http.NotFound)
	err := svc.PauseStream(context.Background(), "t1", "u1")
	if !errors.Is(err, e.ErrorNotFound) {
		t.Fatalf("want ErrorNotFound, got %v", err)
	}
	if got := stop.stops(); len(got) != 0 {
		t.Fatalf("want no stop, got %v", got)
	}
}

// 查 stream_runs 失敗不是「沒有 run」：回錯誤，也不送 stop。
func TestPauseLookupFailureIsAnError(t *testing.T) {
	stop := &stopServer{body: `{"ok":true}`}
	svc := pauseAgent(t, stop, runStore{findErr: errors.New("db down")}, http.NotFound)
	err := svc.PauseStream(context.Background(), "t1", "u1")
	if err == nil || errors.Is(err, e.ErrorNotFound) {
		t.Fatalf("want an internal error, got %v", err)
	}
	if got := stop.stops(); len(got) != 0 {
		t.Fatalf("want no stop, got %v", got)
	}
}

// pen-gpt 記不下 stop（5xx）就是失敗，不能因為本機已停而回成功。
func TestPauseRemoteFailureIsAnError(t *testing.T) {
	stop := &stopServer{status: http.StatusServiceUnavailable}
	svc := pauseAgent(t, stop, runStore{runID: "run-from-db"}, http.NotFound)
	err := svc.PauseStream(context.Background(), "t1", "u1")
	if err == nil || errors.Is(err, e.ErrorNotFound) {
		t.Fatalf("want an internal error, got %v", err)
	}
}

// ok:false（那一輪已經生成完或已結束）：本機也沒有就是 not found。
func TestPauseRemoteNoRunningRunIsNotFound(t *testing.T) {
	stop := &stopServer{body: `{"ok":false}`}
	svc := pauseAgent(t, stop, runStore{runID: "run-from-db"}, http.NotFound)
	if err := svc.PauseStream(context.Background(), "t1", "u1"); !errors.Is(err, e.ErrorNotFound) {
		t.Fatalf("want ErrorNotFound, got %v", err)
	}
}

// 在 pen-gpt 回 header 之前按暫停：等到 run id 才停，停的是這個串流的那一輪，不查 DB。
func TestPauseBeforeHeadersWaitsForTheRunID(t *testing.T) {
	stop := &stopServer{body: `{"ok":true}`}
	finds := 0
	requested := make(chan struct{})
	svc := pauseAgent(t, stop, runStore{runID: "run-from-db", finds: &finds}, func(w http.ResponseWriter, r *http.Request) {
		close(requested)
		time.Sleep(200 * time.Millisecond) // pen-gpt 還在 admission
		w.Header().Set(streamRunIDHeader, testRunID)
		writeChunk(w, `{"type":"text-delta","payload":{"text":"partial"}}`)
		<-r.Context().Done()
	})
	done := make(chan error, 1)
	go func() {
		done <- svc.StreamChat(context.Background(), "u1", &models.StreamRequest{ThreadID: "t1", Query: "q"},
			func(*models.StreamEnvelope) error { return nil })
	}()
	<-requested
	if err := svc.PauseStream(context.Background(), "t1", "u1"); err != nil {
		t.Fatalf("PauseStream: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if got := stop.stops(); len(got) != 1 || got[0] != testRunID {
		t.Fatalf("want one stop for the stream's run, got %v", got)
	}
	if finds != 0 {
		t.Fatalf("want no stream_runs lookup, got %d", finds)
	}
}

// 本機的串流在等待期間被同 thread 的新串流取代：舊的暫停不能取消新串流。
func TestCancelLocalStreamLeavesANewerStream(t *testing.T) {
	svc := NewAgent(fakeStore{}, "http://unused").(*agentService)
	oldCtx, oldCancel := context.WithCancel(context.Background())
	newCtx, newCancel := context.WithCancel(context.Background())
	defer oldCancel()
	defer newCancel()
	older, newer := newStreamHandle(oldCancel), newStreamHandle(newCancel)
	svc.activeStreams["t1"] = newer
	if svc.cancelLocalStream("t1", older) {
		t.Fatal("cancelled a stream that was already replaced")
	}
	if newCtx.Err() != nil || svc.activeStreams["t1"] != newer {
		t.Fatal("the newer stream was touched")
	}
	_ = oldCtx
}
