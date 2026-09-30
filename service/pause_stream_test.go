package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

// startStream runs StreamChat on t1 in the background and returns its result.
func startStream(svc *agentService) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- svc.StreamChat(context.Background(), "u1", &models.StreamRequest{ThreadID: "t1", Query: "q"},
			func(*models.StreamEnvelope) error { return nil })
	}()
	return done
}

// 先讓 pen-gpt 記下 stop，本機串流才斷：/stop 送到時這個串流的連線還開著。
func TestPauseStopsRemotelyBeforeCancellingLocally(t *testing.T) {
	streaming := make(chan context.Context, 1)
	var openAtStop atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/custom/api/windoc/stop" {
			streamCtx := <-streaming
			openAtStop.Store(streamCtx.Err() == nil)
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.Header().Set(streamRunIDHeader, testRunID)
		writeChunk(w, `{"type":"text-delta","payload":{"text":"partial"}}`)
		streaming <- r.Context()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	svc := NewAgent(runStore{}, upstream.URL, "/custom/api/windoc/stream").(*agentService)
	svc.idToken = func() (string, error) { return "", errors.New("no credentials in tests") }

	done := startStream(svc)
	for svc.localStream("t1") == nil || !svc.localStream("t1").settled() {
		time.Sleep(5 * time.Millisecond)
	}
	if err := svc.PauseStream(context.Background(), "t1", "u1"); err != nil {
		t.Fatalf("PauseStream: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if !openAtStop.Load() {
		t.Fatal("the local stream was cancelled before pen-gpt recorded the stop")
	}
}

// pen-gpt 沒記下 stop：本機串流照跑（使用者還拿得到回答），暫停回錯誤。
func TestPauseRemoteFailureKeepsTheLocalStream(t *testing.T) {
	stop := &stopServer{status: http.StatusServiceUnavailable}
	svc := pauseAgent(t, stop, runStore{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(streamRunIDHeader, testRunID)
		writeChunk(w, `{"type":"text-delta","payload":{"text":"partial"}}`)
		<-r.Context().Done()
	})
	done := startStream(svc)
	for svc.localStream("t1") == nil || !svc.localStream("t1").settled() {
		time.Sleep(5 * time.Millisecond)
	}
	h := svc.localStream("t1")
	if err := svc.PauseStream(context.Background(), "t1", "u1"); err == nil || errors.Is(err, e.ErrorNotFound) {
		t.Fatalf("want an internal error, got %v", err)
	}
	if svc.localStream("t1") != h {
		t.Fatal("the local stream was cancelled although the stop failed")
	}
	h.cancel()
	<-done
}

// run id 等不到（pen-gpt 還在 admission）：不查 stream_runs（那裡正在跑的可能已是
// 另一台 BFF 上的下一輪），只斷本機 request，pen-gpt 會在開 run 前停下。
func TestPauseWithoutRunIDCancelsTheLocalRequest(t *testing.T) {
	stop := &stopServer{body: `{"ok":true}`}
	finds := 0
	requested := make(chan struct{})
	svc := pauseAgent(t, stop, runStore{runID: "run-from-db", finds: &finds}, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // lets the server notice the client going away
		close(requested)
		<-r.Context().Done()
	})
	svc.pauseWait = 50 * time.Millisecond
	done := startStream(svc)
	<-requested
	if err := svc.PauseStream(context.Background(), "t1", "u1"); err != nil {
		t.Fatalf("PauseStream: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if got := stop.stops(); len(got) != 0 || finds != 0 {
		t.Fatalf("want no stream_runs lookup and no stop, got %d lookups, stops %v", finds, got)
	}
}

// 暫停等 run id 時同 thread 來了新的串流（使用者按停止後又送出新問題）：新串流是下一輪，
// 暫停不能停它、也不能斷它，也不查 stream_runs（那裡正在跑的就是新的那一輪）。
func TestPauseLeavesAStreamThatReplacedIt(t *testing.T) {
	stop := &stopServer{body: `{"ok":true}`}
	finds := 0
	const newerRunID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	var requests atomic.Int32
	firstRequested := make(chan struct{})
	svc := pauseAgent(t, stop, runStore{runID: newerRunID, finds: &finds}, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // lets the server notice the client going away
		if requests.Add(1) == 1 {
			close(firstRequested)
			<-r.Context().Done() // 還沒回 header 就被新串流取代
			return
		}
		w.Header().Set(streamRunIDHeader, newerRunID)
		writeChunk(w, `{"type":"text-delta","payload":{"text":"partial"}}`)
		<-r.Context().Done()
	})
	first := startStream(svc)
	<-firstRequested
	paused := make(chan error, 1)
	go func() { paused <- svc.PauseStream(context.Background(), "t1", "u1") }()
	time.Sleep(50 * time.Millisecond)
	second := startStream(svc)
	newer := waitForRun(svc, newerRunID)

	if err := <-paused; !errors.Is(err, e.ErrorNotFound) {
		t.Fatalf("want ErrorNotFound, got %v", err)
	}
	<-first
	if got := stop.stops(); len(got) != 0 || finds != 0 {
		t.Fatalf("want no stream_runs lookup and no stop, got %d lookups, stops %v", finds, got)
	}
	if svc.localStream("t1") != newer {
		t.Fatal("the newer stream was cancelled")
	}
	newer.cancel()
	<-second
}

// 暫停已綁定這一輪、/stop 還在路上時新串流取代了它：pen-gpt 回 ok:false（那一輪已被
// supersede），暫停回 not found，新串流不被斷。
func TestPauseReplacedDuringTheStopLeavesTheNewStream(t *testing.T) {
	const newerRunID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	var stopped []string
	var mu sync.Mutex
	var svc *agentService
	var second <-chan error
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/custom/api/windoc/stop" {
			var body struct {
				RunID string `json:"runId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			stopped = append(stopped, body.RunID)
			mu.Unlock()
			second = startStream(svc) // 使用者又送出新問題
			waitForRun(svc, newerRunID)
			w.Write([]byte(`{"ok":false}`))
			return
		}
		io.Copy(io.Discard, r.Body)
		runID := testRunID
		if requests.Add(1) > 1 {
			runID = newerRunID
		}
		w.Header().Set(streamRunIDHeader, runID)
		writeChunk(w, `{"type":"text-delta","payload":{"text":"partial"}}`)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	svc = NewAgent(runStore{}, upstream.URL, "/custom/api/windoc/stream").(*agentService)
	svc.idToken = func() (string, error) { return "", errors.New("no credentials in tests") }

	first := startStream(svc)
	for svc.localStream("t1") == nil || !svc.localStream("t1").settled() {
		time.Sleep(5 * time.Millisecond)
	}
	if err := svc.PauseStream(context.Background(), "t1", "u1"); !errors.Is(err, e.ErrorNotFound) {
		t.Fatalf("want ErrorNotFound, got %v", err)
	}
	<-first
	newer := svc.localStream("t1")
	if newer == nil || !newer.settled() || newer.runID != newerRunID {
		t.Fatal("the newer stream was cancelled")
	}
	mu.Lock()
	if len(stopped) != 1 || stopped[0] != testRunID {
		t.Fatalf("want one stop for the bound run, got %v", stopped)
	}
	mu.Unlock()
	newer.cancel()
	<-second
}

// waitForRun waits until t1's local stream has settled with runID. The run id
// is read only after ready is closed, which orders it after settle's write.
func waitForRun(svc *agentService, runID string) *streamHandle {
	for {
		if h := svc.localStream("t1"); h != nil && h.settled() && h.runID == runID {
			return h
		}
		time.Sleep(5 * time.Millisecond)
	}
}
