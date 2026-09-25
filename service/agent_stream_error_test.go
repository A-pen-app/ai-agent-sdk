package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/A-pen-app/ai-agent-sdk/models"
)

// 上游 error chunk 的原文不能出現在給 client 的 error event。
func TestUpstreamErrorMessageIsFixed(t *testing.T) {
	const leaked = "relation windoc_core.secret_table does not exist"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"error\",\"payload\":{\"error\":%q}}\n\n", leaked)
	}))
	defer upstream.Close()

	svc := NewAgent(nil, upstream.URL, "/custom/api/windoc/stream").(*agentService)
	var events []*models.StreamEnvelope
	writer := func(e *models.StreamEnvelope) error {
		events = append(events, e)
		return nil
	}
	if _, _, err := svc.doUpstreamStream(context.Background(), "u1", &models.StreamRequest{ThreadID: "t1", Query: "q"}, writer); err != nil {
		t.Fatal(err)
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
