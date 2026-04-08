package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/logging"
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
	// error
	Error interface{} `json:"error,omitempty"`
}

// mastraWorkflowEvent represents a workflow event nested inside tool-output payload.output.
type mastraWorkflowEvent struct {
	Type    string                 `json:"type"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

// StreamChat proxies a chat request to the upstream Mastra agent, parses the
// Mastra SSE fullStream protocol, and converts events into the simplified
// BFF SSE format defined in api-proposal.md.
func (svc *agentService) StreamChat(ctx context.Context, userID string, req *models.StreamRequest, writer StreamWriter) error {
	// Create a cancellable context for this stream
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register this stream so it can be cancelled
	svc.streamMutex.Lock()
	svc.activeStreams[req.ThreadID] = cancel
	svc.streamMutex.Unlock()

	// Ensure cleanup when stream ends
	defer func() {
		svc.streamMutex.Lock()
		delete(svc.activeStreams, req.ThreadID)
		svc.streamMutex.Unlock()
	}()

	// Send start event.
	if err := writer(&models.StreamEnvelope{Event: models.StreamEventStart, Data: struct{}{}}); err != nil {
		return err
	}

	// Execute the upstream stream; collect references and any error.
	refs, streamErr := svc.doUpstreamStream(streamCtx, userID, req, writer)

	// Always send accumulated references, finish, and done — even on error —
	// so the client can properly clean up its streaming state.
	if len(refs) > 0 {
		_ = writer(&models.StreamEnvelope{Event: models.StreamEventReferences, Data: refs})
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

	return streamErr
}

// doUpstreamStream handles the actual upstream request and stream parsing.
// It returns collected references and any error encountered.
func (svc *agentService) doUpstreamStream(ctx context.Context, userID string, req *models.StreamRequest, writer StreamWriter) ([]models.Reference, error) {
	// Build upstream request to pen-gpt Mastra agent.
	//
	// We hit the custom /custom/api/chat/stream endpoint (not Mastra's
	// built-in /api/agents/:agentId/stream) so that PauseStream can abort
	// the running agent.stream() out-of-band via /custom/api/chat/stop.
	// The custom endpoint forwards the same SSE-wrapped JSON chunk format
	// as the built-in route, so the parser below is unchanged.
	mastraURL := svc.agentStreamURL + "/custom/api/chat/stream"
	body := map[string]interface{}{
		"messages": []map[string]string{{"role": "user", "content": req.Query}},
		"memory": map[string]interface{}{
			"resource": userID,
			"thread":   req.ThreadID,
		},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		sendStreamError(writer, "INTERNAL_ERROR", "failed to build upstream request")
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, mastraURL, bytes.NewReader(bodyBytes))
	if err != nil {
		sendStreamError(writer, "INTERNAL_ERROR", "failed to create upstream request")
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-user-id", userID)

	// Attach Google ID token for Cloud Run authentication.
	tokenSource, err := idtoken.NewTokenSource(ctx, svc.agentStreamURL)
	if err != nil {
		logging.Errorw(ctx, "Failed to create ID token source", "error", err)
		sendStreamError(writer, "INTERNAL_ERROR", "failed to authenticate with AI service")
		return nil, err
	}
	token, err := tokenSource.Token()
	if err != nil {
		logging.Errorw(ctx, "Failed to get ID token", "error", err)
		sendStreamError(writer, "INTERNAL_ERROR", "failed to authenticate with AI service")
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token.AccessToken)

	resp, err := svc.httpClient.Do(httpReq)
	if err != nil {
		sendStreamError(writer, "UPSTREAM_ERROR", "AI 服務暫時無法使用，請稍後再試")
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		sendStreamError(writer, "UPSTREAM_ERROR", fmt.Sprintf("AI 服務回傳錯誤 (status %d)", resp.StatusCode))
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	// Parse the upstream SSE stream line by line.
	// Mastra modern /stream returns SSE-wrapped JSON chunks where all data
	// is nested inside a "payload" object:
	//   data: {"type":"text-delta","runId":"...","from":"AGENT","payload":{"text":"..."}}
	var refs []models.Reference
	var inThought bool

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
			msg := "AI 服務發生錯誤"
			if s, ok := chunk.Payload.Error.(string); ok && s != "" {
				msg = s
			}
			sendStreamError(writer, "UPSTREAM_ERROR", msg)

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

	if err := scanner.Err(); err != nil {
		logging.Error(ctx, "stream scanner error: %v", err)
	}

	return refs, nil
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
	stopURL := svc.agentStreamURL + "/custom/api/chat/stop"

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

	// Same Cloud Run ID token flow as doUpstreamStream — the stop endpoint
	// sits behind the same Cloud Run service auth.
	tokenSource, err := idtoken.NewTokenSource(ctx, svc.agentStreamURL)
	if err != nil {
		return false, fmt.Errorf("create id token source: %w", err)
	}
	token, err := tokenSource.Token()
	if err != nil {
		return false, fmt.Errorf("get id token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

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

// sendStreamError sends an error event to the client.
func sendStreamError(writer StreamWriter, code, message string) {
	_ = writer(&models.StreamEnvelope{
		Event: models.StreamEventError,
		Data:  models.StreamErrorData{Code: code, Message: message},
	})
}
