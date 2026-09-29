package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/logging"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
)

// mastraChunk represents a single SSE JSON chunk from Mastra's /stream endpoint.
// All data is nested inside the Payload field.
type mastraChunk struct {
	Type    string             `json:"type"`
	Payload mastraChunkPayload `json:"payload"`
}

type mastraChunkPayload struct {
	// text-delta
	Text string `json:"text,omitempty"`
	// tool-call / tool-result
	ToolCallID string      `json:"toolCallId,omitempty"`
	ToolName   string      `json:"toolName,omitempty"`
	Args       interface{} `json:"args,omitempty"`
	Result     interface{} `json:"result,omitempty"`
	// tool-output (workflow events nested inside output)
	Output json.RawMessage `json:"output,omitempty"`
	// error：只寫進 server log，不轉給 client
	Error json.RawMessage `json:"error,omitempty"`
}

// ErrClientNotified 表示 StreamChat 失敗，且已經送過 error event 給 client；
// 呼叫端用 errors.Is 判斷，只記 log，不要再補送一次 error event。
var ErrClientNotified = errors.New("stream failed, client already notified")

// upstreamLogLimit 限制寫進 log 的上游錯誤內容長度。
const upstreamLogLimit = 2048

// mastraWorkflowEvent represents a workflow event nested inside tool-output payload.output.
type mastraWorkflowEvent struct {
	Type    string                 `json:"type"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

// StreamChat proxies a chat request to the upstream Mastra agent, parses the
// Mastra SSE fullStream protocol, and converts events into the simplified
// BFF SSE format defined in api-proposal.md.
// 串流失敗時回傳 wrap 過的 ErrClientNotified，client 已收到 error event；
// 被停止（PauseStream、同 thread 新串流、client 離開）不算失敗，回傳 nil。
func (svc *agentService) StreamChat(ctx context.Context, userID string, req *models.StreamRequest, writer StreamWriter) error {
	// Create a cancellable context for this stream
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register this stream so it can be cancelled. One active stream per
	// thread: a new stream supersedes (cancels) any stream still running on
	// the same thread, so its cancel handle is never silently lost.
	h := &streamHandle{cancel: cancel}
	svc.streamMutex.Lock()
	if old, ok := svc.activeStreams[req.ThreadID]; ok {
		old.cancel()
	}
	svc.activeStreams[req.ThreadID] = h
	svc.streamMutex.Unlock()

	// Ensure cleanup when stream ends — but only deregister our own entry;
	// a newer stream may have replaced it while we were finishing.
	defer func() {
		svc.streamMutex.Lock()
		if svc.activeStreams[req.ThreadID] == h {
			delete(svc.activeStreams, req.ThreadID)
		}
		svc.streamMutex.Unlock()
	}()

	// Send start event.
	if err := writer(&models.StreamEnvelope{Event: models.StreamEventStart, Data: struct{}{}}); err != nil {
		return err
	}

	// Execute the upstream stream; collect references/recommendations and any error.
	refs, recs, streamErr := svc.doUpstreamStream(streamCtx, userID, req, writer)

	// Always send accumulated references, recommendations, finish, and done —
	// even on error — so the client can properly clean up its streaming state.
	if len(refs) > 0 {
		_ = writer(&models.StreamEnvelope{Event: models.StreamEventReferences, Data: refs})
	}
	if len(recs) > 0 {
		_ = writer(&models.StreamEnvelope{Event: models.StreamEventRecommendations, Data: recs})
	}

	// Send finish with the full message list from DB.
	messages, err := svc.ListMessages(streamCtx, req.ThreadID, userID, "", 100)
	if err != nil {
		logging.Error(streamCtx, "failed to list messages for finish event: %v", err)
		_ = writer(&models.StreamEnvelope{Event: models.StreamEventFinish, Data: []models.MessageResponse{}})
	} else {
		_ = writer(&models.StreamEnvelope{Event: models.StreamEventFinish, Data: messages.Data})
	}

	// Send done.
	_ = writer(&models.StreamEnvelope{Event: models.StreamEventDone, Data: struct{}{}})

	if streamErr != nil {
		return fmt.Errorf("%w: %w", ErrClientNotified, streamErr)
	}
	return nil
}

// userContent 組出使用者訊息的 content：純文字時是字串（維持原本的 body 形狀），
// 有帶圖時改成 AI SDK 的 message parts 陣列，pen-gpt 端原樣轉給 agent.stream()。
//
// mediaType 固定 image/jpeg —— 接入服務（windoc-api）上傳時一律重新編碼成 JPEG；
// ponytail: 哪天允許原檔上傳，就把 media type 一起從 ImageURLs 帶過來。
func userContent(req *models.StreamRequest) any {
	if len(req.ImageURLs) == 0 {
		return req.Query
	}
	parts := make([]map[string]any, 0, len(req.ImageURLs)+1)
	parts = append(parts, map[string]any{"type": "text", "text": req.Query})
	for _, u := range req.ImageURLs {
		parts = append(parts, map[string]any{"type": "file", "data": u, "mediaType": "image/jpeg"})
	}
	return parts
}

// doUpstreamStream handles the actual upstream request and stream parsing.
// It returns collected references, raw recommendations, and any error encountered.
// 回傳 error 時一定已經送過一次 error event 給 client。
func (svc *agentService) doUpstreamStream(ctx context.Context, userID string, req *models.StreamRequest, writer StreamWriter) ([]models.Reference, []json.RawMessage, error) {
	// Build upstream request to pen-gpt Mastra agent.
	//
	// We hit the custom stream endpoint（預設 /custom/api/chat/stream，可由
	// NewAgent 的 streamPath 覆寫，not Mastra's built-in
	// /api/agents/:agentId/stream) so that PauseStream can abort
	// the running agent.stream() out-of-band via /custom/api/chat/stop.
	// The custom endpoint forwards the same SSE-wrapped JSON chunk format
	// as the built-in route, so the parser below is unchanged.
	mastraURL := svc.agentStreamURL + svc.streamPath
	body := map[string]interface{}{
		"messages": []map[string]any{{"role": "user", "content": userContent(req)}},
		"memory": map[string]interface{}{
			"resource": userID,
			"thread":   req.ThreadID,
		},
	}
	// 使用者座標（選帶）：pen-gpt 端以 body.location 讀取，用於地點相關推薦。
	if req.Latitude != nil && req.Longitude != nil {
		body["location"] = map[string]float64{
			"latitude":  *req.Latitude,
			"longitude": *req.Longitude,
		}
	}
	// 預設地區（選帶）：無座標時的地點參考，pen-gpt 端以 body.default_location 讀取。
	if req.DefaultLocation != "" {
		body["default_location"] = req.DefaultLocation
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		sendStreamError(writer, "INTERNAL_ERROR", "failed to build upstream request")
		return nil, nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, mastraURL, bytes.NewReader(bodyBytes))
	if err != nil {
		sendStreamError(writer, "INTERNAL_ERROR", "failed to create upstream request")
		return nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-user-id", userID)

	svc.setIDToken(ctx, httpReq)

	resp, err := svc.httpClient.Do(httpReq)
	if err != nil {
		if stopped(ctx) {
			logging.Infow(ctx, "upstream stream cancelled before response", "thread_id", req.ThreadID)
			return nil, nil, nil
		}
		logging.Errorw(ctx, "upstream stream request failed", "error", err.Error(), "thread_id", req.ThreadID)
		sendStreamError(writer, "UPSTREAM_ERROR", "AI 服務暫時無法使用，請稍後再試")
		return nil, nil, fmt.Errorf("upstream stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamLogLimit))
		logging.Errorw(ctx, "upstream stream returned non-OK status",
			"status_code", resp.StatusCode,
			"body", logSnippet(detail),
			"thread_id", req.ThreadID)
		sendStreamError(writer, "UPSTREAM_ERROR", "AI 服務發生錯誤")
		return nil, nil, fmt.Errorf("upstream stream returned status %d", resp.StatusCode)
	}

	// Parse the upstream SSE stream line by line.
	// Mastra modern /stream returns SSE-wrapped JSON chunks where all data
	// is nested inside a "payload" object:
	//   data: {"type":"text-delta","runId":"...","from":"AGENT","payload":{"text":"..."}}
	var refs []models.Reference
	var recs []json.RawMessage
	var inThought bool
	var upstreamErr error

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // up to 1MB per line for large tool results

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		// Strip SSE "data: " or "data:" prefix; skip non-data lines (e.g. "event:", comments).
		if strings.HasPrefix(line, "data: ") {
			line = line[6:]
		} else if strings.HasPrefix(line, "data:") {
			line = line[5:]
		} else {
			continue
		}

		// Skip SSE stream termination sentinel.
		if line == "[DONE]" {
			continue
		}

		// Parse the Mastra JSON chunk.
		var chunk mastraChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}

		// error chunk 之後照樣讀到上游關閉，讓 pen-gpt 收完這回合再送 finish；
		// 只是不再轉給 client。
		if upstreamErr != nil && chunk.Type != "error" {
			continue
		}

		switch chunk.Type {
		case "text-delta":
			if chunk.Payload.Text == "" {
				continue
			}
			// Filter thought/reasoning content enclosed in <think>...</think>.
			text, updated := filterThought(chunk.Payload.Text, inThought)
			inThought = updated
			if text != "" {
				_ = writer(&models.StreamEnvelope{
					Event: models.StreamEventTextDelta,
					Data:  models.TextDeltaData{Text: text},
				})
			}

		case "tool-call":
			// Only emit workflow steps for non-workflow tools to avoid duplicates.
			// Workflow steps will be emitted when actually executed via tool-output events.
			if !strings.HasPrefix(chunk.Payload.ToolName, "workflow-") && chunk.Payload.ToolName != "" {
				displayName := translateToolName(chunk.Payload.ToolName, chunk.Payload.Args)
				_ = writer(&models.StreamEnvelope{
					Event: models.StreamEventWorkflowStep,
					Data:  models.WorkflowStep{ID: chunk.Payload.ToolCallID, DisplayName: displayName},
				})
			}

		case "tool-result":
			if chunk.Payload.Result == nil {
				continue
			}
			extracted := extractReferences(chunk.Payload.Result)
			refs = append(refs, extracted...)
			recs = append(recs, extractRecommendations(chunk.Payload.Result)...)

		case "tool-output":
			// tool-output contains nested workflow events in payload.output.
			// Extract workflow-step-start events to emit workflow_step to the client.
			if len(chunk.Payload.Output) == 0 {
				continue
			}
			var wfEvent mastraWorkflowEvent
			if err := json.Unmarshal(chunk.Payload.Output, &wfEvent); err != nil {
				continue
			}
			if wfEvent.Type == "workflow-step-start" && wfEvent.Payload != nil {
				stepName, _ := wfEvent.Payload["stepName"].(string)
				stepID, _ := wfEvent.Payload["id"].(string)
				if stepName == "" {
					continue
				}
				// Extract query from nested payload for display name translation.
				var query string
				if inner, ok := wfEvent.Payload["payload"].(map[string]interface{}); ok {
					query, _ = inner["query"].(string)
				}
				displayName := translateStepName(stepName, query)
				if displayName == "" {
					continue // Skip unrecognized steps.
				}
				_ = writer(&models.StreamEnvelope{
					Event: models.StreamEventWorkflowStep,
					Data:  models.WorkflowStep{ID: stepID, DisplayName: displayName},
				})
			}

		case "error":
			logging.Errorw(ctx, "upstream stream error chunk",
				"error", logSnippet(chunk.Payload.Error),
				"thread_id", req.ThreadID)
			if upstreamErr == nil {
				sendStreamError(writer, "UPSTREAM_ERROR", "AI 服務發生錯誤")
				upstreamErr = errors.New("upstream stream error chunk")
			}

			// Types we intentionally skip:
			// "start"                           — stream start metadata
			// "step-start"                      — LLM step start
			// "text-start" / "text-end"         — text block boundaries
			// "tool-call-input-streaming-start"  — partial tool call start
			// "tool-call-delta"                 — streaming args
			// "tool-call-input-streaming-end"    — partial tool call end
			// "step-finish"                     — step boundary
			// "finish"                          — stream end metadata
		}
	}

	if err := scanner.Err(); err != nil && upstreamErr == nil && !stopped(ctx) {
		logging.Errorw(ctx, "upstream stream read failed", "error", err.Error(), "thread_id", req.ThreadID)
		sendStreamError(writer, "UPSTREAM_ERROR", "AI 服務發生錯誤")
		return refs, recs, fmt.Errorf("read upstream stream: %w", err)
	}

	return refs, recs, upstreamErr
}

// stopped 表示串流是被停止的（PauseStream、同 thread 新串流、client 離開），不是上游失敗。
func stopped(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// callRemoteStop calls Mastra's /custom/api/chat/stop endpoint to abort an
// in-flight agent.stream() for the given thread. This is the application-
// layer cancel signal that PauseStream relies on, since Cloud Run + HTTP/1.1
// will not propagate the local TCP close to the Mastra container.
//
// Returns:
//   - (true, nil)  : upstream confirmed an active stream was aborted
//   - (false, nil) : upstream returned 200 but had no matching stream
//   - (false, err) : transport, auth, or non-200 response
func (svc *agentService) callRemoteStop(ctx context.Context, threadID, userID string) (bool, error) {
	// stop 路徑跟著 streamPath 走：/custom/api/{ns}/stream → /custom/api/{ns}/stop
	// （chat 與 windoc 皆同此慣例；abort registry 以 ns 隔命名空間，打錯 ns 停不掉）。
	stopURL := svc.agentStreamURL + strings.TrimSuffix(svc.streamPath, "/stream") + "/stop"

	body, err := json.Marshal(map[string]string{"threadId": threadID})
	if err != nil {
		return false, fmt.Errorf("marshal stop body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stopURL, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("create stop request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-user-id", userID)

	svc.setIDToken(ctx, req)

	resp, err := svc.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("call upstream stop: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("upstream stop returned status %d", resp.StatusCode)
	}

	var result struct {
		Ok bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("decode stop response: %w", err)
	}
	return result.Ok, nil
}

// filterThought strips <think>...</think> content from text deltas.
// It returns the filtered text and the updated inThought state.
func filterThought(text string, inThought bool) (string, bool) {
	var result strings.Builder

	for len(text) > 0 {
		if inThought {
			idx := strings.Index(text, "</think>")
			if idx == -1 {
				// Still inside thought block — discard all remaining text.
				return result.String(), true
			}
			text = text[idx+len("</think>"):]
			inThought = false
			continue
		}

		idx := strings.Index(text, "<think>")
		if idx == -1 {
			result.WriteString(text)
			return result.String(), false
		}
		result.WriteString(text[:idx])
		text = text[idx+len("<think>"):]
		inThought = true
	}

	return result.String(), inThought
}

// setIDToken attaches a Google ID token for Cloud Run authentication.
// 本機 authorized_user 憑證拿不到 ID token，照樣送出；在 Cloud Run 上看到這行 log，
// 後面的 401/403 就是它造成的。
func (svc *agentService) setIDToken(ctx context.Context, req *http.Request) {
	token, err := svc.idToken()
	if err != nil {
		logging.Warn(ctx, "sending upstream request without Cloud Run ID token: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
}

// cloudRunIDTokenSource 重用同一個 idtoken source：它內建 ReuseTokenSource，
// token 到期前不會再打 metadata server。建立失敗不快取，下一次請求重試。
type cloudRunIDTokenSource struct {
	audience string
	mu       sync.Mutex
	ts       oauth2.TokenSource
}

func (s *cloudRunIDTokenSource) token() (string, error) {
	s.mu.Lock()
	if s.ts == nil {
		// source 會跨請求重用，不能綁在單次請求的 ctx 上。
		ts, err := idtoken.NewTokenSource(context.Background(), s.audience)
		if err != nil {
			s.mu.Unlock()
			return "", err
		}
		s.ts = ts
	}
	ts := s.ts
	s.mu.Unlock()

	token, err := ts.Token()
	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

// logSnippet 截到 upstreamLogLimit，並去掉被截斷的 UTF-8 字元，避免 log 出現亂碼。
func logSnippet(b []byte) string {
	if len(b) > upstreamLogLimit {
		b = b[:upstreamLogLimit]
	}
	return strings.ToValidUTF8(string(b), "")
}

// sendStreamError sends an error event to the client.
func sendStreamError(writer StreamWriter, code, message string) {
	_ = writer(&models.StreamEnvelope{
		Event: models.StreamEventError,
		Data:  models.StreamErrorData{Code: code, Message: message},
	})
}
