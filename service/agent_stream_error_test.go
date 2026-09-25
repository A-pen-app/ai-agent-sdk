package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/logging"
)

func TestMain(m *testing.M) {
	if err := logging.Initialize(nil); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

const leaked = "relation windoc_core.secret_table does not exist"

func runUpstream(t *testing.T, handler http.HandlerFunc) ([]*models.StreamEnvelope, error) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	defer upstream.Close()

	svc := NewAgent(nil, upstream.URL, "/custom/api/windoc/stream").(*agentService)
	svc.idToken = func(context.Context, string) (string, error) { return "", errors.New("no credentials in tests") }
	var events []*models.StreamEnvelope
	writer := func(e *models.StreamEnvelope) error {
		events = append(events, e)
		return nil
	}
	_, _, err := svc.doUpstreamStream(context.Background(), "u1", &models.StreamRequest{ThreadID: "t1", Query: "q"}, writer)
	return events, err
}

func assertSingleFixedError(t *testing.T, events []*models.StreamEnvelope, err error) {
	t.Helper()
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("want ErrUpstream, got %v", err)
	}
	if len(events) != 1 || events[0].Event != models.StreamEventError {
		t.Fatalf("want one error event, got %#v", events)
	}
	data := events[0].Data.(models.StreamErrorData)
	if data.Code != "UPSTREAM_ERROR" || data.Message != "AI 服務發生錯誤" {
		t.Fatalf("unexpected error event: %#v", data)
	}
	if strings.Contains(data.Message, leaked) {
		t.Fatal("upstream error text leaked to client")
	}
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
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"error\",\"payload\":{\"error\":%s}}\n\n", payload)
			})
			assertSingleFixedError(t, events, err)
		})
	}
}

// 第一個 error chunk 之後的 chunk（含重複的 error）都不轉給 client。
func TestUpstreamErrorChunkEndsStream(t *testing.T) {
	events, err := runUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"error\",\"payload\":{\"error\":\"first\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"error\",\"payload\":{\"error\":\"second\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"text-delta\",\"payload\":{\"text\":\"after\"}}\n\n")
	})
	assertSingleFixedError(t, events, err)
}

func TestUpstreamNonOKStatusIsFixed(t *testing.T) {
	events, err := runUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, leaked, http.StatusServiceUnavailable)
	})
	assertSingleFixedError(t, events, err)
}
