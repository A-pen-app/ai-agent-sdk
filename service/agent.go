package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	e "github.com/A-pen-app/errors"
	"github.com/A-pen-app/logging"
	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/ai-agent-sdk/store"
	"github.com/google/uuid"
)

type agentService struct {
	s              store.Agent
	agentStreamURL string
	httpClient     *http.Client
	// Stream management
	activeStreams map[string]context.CancelFunc // threadID -> cancel function
	streamMutex   sync.RWMutex
}

// NewAgent creates a new Agent service.
func NewAgent(s store.Agent, agentStreamURL string) Agent {
	return &agentService{
		s:              s,
		agentStreamURL: agentStreamURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
		activeStreams: make(map[string]context.CancelFunc),
	}
}

func (svc *agentService) ListThreads(ctx context.Context, userID, cursor string, count int) (*models.ThreadListResponse, error) {
	rows, err := svc.s.ListThreads(ctx, userID, cursor, count)
	if err != nil {
		return nil, err
	}
	return buildThreadListResponse(rows, count), nil
}

func (svc *agentService) SearchThreads(ctx context.Context, userID, keyword, cursor string, count int) (*models.ThreadListResponse, error) {
	rows, err := svc.s.SearchThreads(ctx, userID, keyword, cursor, count)
	if err != nil {
		return nil, err
	}
	return buildThreadListResponse(rows, count), nil
}

func (svc *agentService) CreateThread(ctx context.Context, userID, query string) (*models.ThreadResponse, error) {
	now := time.Now().UTC()
	thread := &models.MastraThread{
		ID:         uuid.New().String(),
		ResourceID: userID,
		Title:      query,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := svc.s.CreateThread(ctx, thread); err != nil {
		return nil, err
	}
	return &models.ThreadResponse{
		ID:       thread.ID,
		Title:    thread.Title,
		IsPinned: false,
	}, nil
}

func (svc *agentService) GetThread(ctx context.Context, threadID, userID string) (*models.ThreadResponse, error) {
	threadWithPin, err := svc.s.GetThread(ctx, threadID, userID)
	if err != nil {
		return nil, err
	}
	return &models.ThreadResponse{
		ID:       threadWithPin.ID,
		Title:    threadWithPin.Title,
		IsPinned: threadWithPin.IsPinned,
	}, nil
}

func (svc *agentService) DeleteThread(ctx context.Context, threadID, userID string) error {
	return svc.s.DeleteThread(ctx, threadID, userID)
}

func (svc *agentService) UpdateThread(ctx context.Context, threadID, userID, title string) (*models.ThreadResponse, error) {
	if err := svc.s.UpdateThread(ctx, threadID, userID, title); err != nil {
		return nil, err
	}
	return &models.ThreadResponse{
		ID:    threadID,
		Title: title,
	}, nil
}

func (svc *agentService) UpdateThreadPin(ctx context.Context, userID, threadID string, isPinned bool) error {
	return svc.s.UpdateThreadPin(ctx, userID, threadID, isPinned)
}

func (svc *agentService) ListMessages(ctx context.Context, threadID, userID, cursor string, count int) (*models.MessageListResponse, error) {
	rows, err := svc.s.ListMessages(ctx, threadID, userID, cursor, count)
	if err != nil {
		return nil, err
	}

	hasMore := len(rows) > count
	if hasMore {
		rows = rows[:count]
	}

	data := make([]models.MessageResponse, 0, len(rows))
	for _, row := range rows {
		msg := parseMessage(row)
		data = append(data, msg)
	}

	resp := &models.MessageListResponse{Data: data}
	if hasMore {
		last := data[len(data)-1].ID
		resp.Next = &last
	}

	// Reverse to chronological order (ASC) for display
	for i, j := 0, len(resp.Data)-1; i < j; i, j = i+1, j-1 {
		resp.Data[i], resp.Data[j] = resp.Data[j], resp.Data[i]
	}
	return resp, nil
}

func (svc *agentService) UpsertFeedback(ctx context.Context, userID, messageID, feedback string) error {
	return svc.s.UpsertFeedback(ctx, userID, messageID, feedback)
}

// buildThreadListResponse trims the fetch-N+1 result and sets the next cursor.
func buildThreadListResponse(rows []models.ThreadWithPin, count int) *models.ThreadListResponse {
	hasMore := len(rows) > count
	if hasMore {
		rows = rows[:count]
	}

	data := make([]models.ThreadResponse, 0, len(rows))
	for _, r := range rows {
		data = append(data, models.ThreadResponse{
			ID:       r.ID,
			Title:    r.Title,
			IsPinned: r.IsPinned,
		})
	}

	resp := &models.ThreadListResponse{Data: data}
	if hasMore {
		last := data[len(data)-1].ID
		resp.Next = &last
	}
	return resp
}

// parseMessage converts a MessageWithFeedback row to a MessageResponse.
// All messages use V2 format: {"format":2,"parts":[...]}
func parseMessage(row models.MessageWithFeedback) models.MessageResponse {
	// For assistant messages, default feedback to "none" so the field is
	// always present in the JSON response and the client can render the
	// feedback UI unconditionally.
	feedback := row.Feedback
	if row.Role == "assistant" && feedback == nil {
		none := "none"
		feedback = &none
	}

	msg := models.MessageResponse{
		ID:       row.ID,
		Role:     row.Role,
		Feedback: feedback,
	}

	var v2 models.MastraContentV2
	if err := json.Unmarshal([]byte(row.Content), &v2); err != nil || v2.Format == 0 {
		// Not V2 format — return raw content as-is.
		msg.Content = row.Content
		return msg
	}

	var textParts []string
	var steps []models.WorkflowStep
	var refs []models.Reference
	var hasResults *bool

	for _, part := range v2.Parts {
		switch part.Type {
		case "text":
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		case "tool-invocation":
			if part.ToolInvocation == nil {
				continue
			}
			ti := part.ToolInvocation
			// Extract inner workflow steps from result if available.
			if ti.State == "result" && ti.Result != nil {
				if strings.HasPrefix(ti.ToolName, "workflow-") {
					// Workflow tool - extract inner steps
					innerSteps := extractWorkflowSteps(ti.Args, ti.Result)
					if len(innerSteps) > 0 {
						steps = append(steps, innerSteps...)
					}
				} else if ti.ToolName != "" {
					// Non-workflow tool - add as single step with translated name
					displayName := translateToolName(ti.ToolName, ti.Args)
					steps = append(steps, models.WorkflowStep{
						ID:          ti.ToolCallID,
						DisplayName: displayName,
					})
				}
				refs = append(refs, extractReferences(ti.Result)...)
				if hasResults == nil {
					hasResults = extractHasResults(ti.Result)
				}
			}
		}
	}

	msg.Content = strings.Join(textParts, "\n")
	if len(steps) > 0 {
		msg.WorkflowSteps = steps
	}
	if len(refs) > 0 {
		msg.References = refs
	}
	if hasResults != nil {
		msg.HasResults = hasResults
	}
	return msg
}

// workflowStepDef maps a Mastra workflow step to its display name and execute flag.
type workflowStepDef struct {
	StepName    string
	DisplayTpl  string // Chinese display template; %s is replaced with the query.
	ExecuteFlag string // Key in workflow args (empty = always shown).
}

// workflowStepDefs defines the inner steps we expose, in display order.
// Display text must match iOS Localizable.xcstrings ai_step_* keys (zh-Hant).
var workflowStepDefs = []workflowStepDef{
	{"query-medical-step", "查詢關於%s的醫學文獻資訊...", "executeMedical"},
	{"query-drug-step", "查詢關於%s的藥品交互作用與仿單資訊...", "executeDrug"},
	{"query-insite-step", "檢索 A-Pen 中關於%s的社群內容...", "executeInsite"},
	{"query-law-step", "查詢關於%s的相關醫事法規與法律...", "executeLaw"},
	{"query-lecture-step", "檢索 A-Pen 中關於%s的講座內容...", "executeLecture"},
	{"query-nhi-regulation-step", "查詢關於%s的健保支付準則與藥價規範...", "executeNhiRegulation"},
	{"query-finance-step", "查詢關於%s的理財市場趨勢與資產配置相關資訊...", "executeFinance"},
	{"summary-step", "正在彙整資訊中...", ""},
}

// translateStepName returns the Chinese display name for a known workflow step.
// Returns empty string if the step is not recognized.
func translateStepName(stepName, query string) string {
	for _, def := range workflowStepDefs {
		if def.StepName == stepName {
			if strings.Contains(def.DisplayTpl, "%s") {
				return fmt.Sprintf(def.DisplayTpl, query)
			}
			return def.DisplayTpl
		}
	}
	return ""
}

// translateToolName returns the Chinese display name for a known tool.
func translateToolName(toolName string, args interface{}) string {
	// Extract query from args if available
	var query string
	if args != nil {
		if argsData, err := json.Marshal(args); err == nil {
			var argsMap map[string]interface{}
			if json.Unmarshal(argsData, &argsMap) == nil {
				if q, ok := argsMap["query"].(string); ok {
					query = q
				} else if q, ok := argsMap["prompt"].(string); ok {
					query = q
				}
			}
		}
	}
	
	switch toolName {
	case "agent-financeSubAgent":
		if query != "" {
			return fmt.Sprintf("查詢關於%s的理財市場趨勢與資產配置相關資訊", query)
		}
		return "查詢理財市場趨勢與資產配置相關資訊"
	case "agent-consultationProcessAgent":
		if query != "" {
			return fmt.Sprintf("查詢關於%s的相關醫學資訊中", query)
		}
		return "查詢專業醫療資訊"
	default:
		// Return the original tool name if no translation is available
		return toolName
	}
}

// extractWorkflowSteps extracts the inner steps from a workflow tool invocation,
// returning human-readable Chinese display names with the query inserted.
//
// When result is non-nil (message list path), steps are validated against the
// result to confirm they actually exist. When result is nil (streaming path),
// steps are derived purely from the execute flags in args.
func extractWorkflowSteps(args, result interface{}) []models.WorkflowStep {
	// Parse args to get query and execute flags.
	argsData, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	var wfArgs struct {
		Query                string `json:"query"`
		ExecuteMedical       bool   `json:"executeMedical"`
		ExecuteDrug          bool   `json:"executeDrug"`
		ExecuteInsite        bool   `json:"executeInsite"`
		ExecuteLaw           bool   `json:"executeLaw"`
		ExecuteLecture       bool   `json:"executeLecture"`
		ExecuteNhiRegulation bool   `json:"executeNhiRegulation"`
		ExecuteFinance       bool   `json:"executeFinance"`
		// New format with nested inputData
		InputData struct {
			Query                string `json:"query"`
			ExecuteMedical       bool   `json:"executeMedical"`
			ExecuteDrug          bool   `json:"executeDrug"`
			ExecuteInsite        bool   `json:"executeInsite"`
			ExecuteLaw           bool   `json:"executeLaw"`
			ExecuteLecture       bool   `json:"executeLecture"`
			ExecuteNhiRegulation bool   `json:"executeNhiRegulation"`
			ExecuteFinance       bool   `json:"executeFinance"`
		} `json:"inputData"`
	}
	if err := json.Unmarshal(argsData, &wfArgs); err != nil {
		return nil
	}

	// Determine which format to use (new format has nested inputData)
	var query string
	var execFlags map[string]bool
	
	if wfArgs.InputData.Query != "" {
		// New format with inputData
		query = wfArgs.InputData.Query
		execFlags = map[string]bool{
			"executeMedical":       wfArgs.InputData.ExecuteMedical,
			"executeDrug":          wfArgs.InputData.ExecuteDrug,
			"executeInsite":        wfArgs.InputData.ExecuteInsite,
			"executeLaw":           wfArgs.InputData.ExecuteLaw,
			"executeLecture":       wfArgs.InputData.ExecuteLecture,
			"executeNhiRegulation": wfArgs.InputData.ExecuteNhiRegulation,
			"executeFinance":       wfArgs.InputData.ExecuteFinance,
		}
	} else {
		// Old format with direct fields
		query = wfArgs.Query
		execFlags = map[string]bool{
			"executeMedical":       wfArgs.ExecuteMedical,
			"executeDrug":          wfArgs.ExecuteDrug,
			"executeInsite":        wfArgs.ExecuteInsite,
			"executeLaw":           wfArgs.ExecuteLaw,
			"executeLecture":       wfArgs.ExecuteLecture,
			"executeNhiRegulation": wfArgs.ExecuteNhiRegulation,
			"executeFinance":       wfArgs.ExecuteFinance,
		}
	}

	// Try to get steps from new format first
	var resultSteps map[string]json.RawMessage
	var hasNewFormat bool
	
	if result != nil {
		resultData, err := json.Marshal(result)
		if err == nil {
			// Try new format - direct result.result structure
			var newFormat struct {
				Result struct {
					Summary         string `json:"summary"`
					FinalReferences []json.RawMessage `json:"final_references"`
					HasResults      bool `json:"hasResults"`
				} `json:"result"`
			}
			if json.Unmarshal(resultData, &newFormat) == nil && newFormat.Result.HasResults {
				hasNewFormat = true
			} else {
				// Fallback to old format
				var outer struct {
					Result struct {
						Steps map[string]json.RawMessage `json:"steps"`
					} `json:"result"`
				}
				if json.Unmarshal(resultData, &outer) == nil {
					resultSteps = outer.Result.Steps
				}
			}
		}
	}

	var steps []models.WorkflowStep
	for _, def := range workflowStepDefs {
		// For new format, only check execute flags (no step existence check)
		if hasNewFormat {
			// Skip steps whose execute flag is false
			if def.ExecuteFlag != "" && !execFlags[def.ExecuteFlag] {
				continue
			}
		} else {
			// For old format, check both step existence and execute flags
			if resultSteps != nil {
				if _, ok := resultSteps[def.StepName]; !ok {
					continue
				}
			}
			if def.ExecuteFlag != "" && !execFlags[def.ExecuteFlag] {
				continue
			}
		}
		
		displayName := translateStepName(def.StepName, query)
		if displayName == "" {
			continue
		}
		steps = append(steps, models.WorkflowStep{
			ID:          def.StepName,
			DisplayName: displayName,
		})
	}
	return steps
}

// extractReferences extracts Reference objects from a tool invocation result.
//
// Primary source: summary-step.output.final_references (aggregated list).
// Fallback: collect from each query-*-step.output.references.
//
// Each reference object may contain: type, id, title, url, post_id,
// product_name_zh, product_name_en.
func extractReferences(result interface{}) []models.Reference {
	if result == nil {
		return nil
	}

	data, err := json.Marshal(result)
	if err != nil {
		return nil
	}

	// Try new format first - direct final_references in result.result
	var newFormat struct {
		Result struct {
			FinalReferences []refJSON `json:"final_references"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &newFormat); err == nil && len(newFormat.Result.FinalReferences) > 0 {
		return deduplicateRefs(newFormat.Result.FinalReferences)
	}

	// Fallback to old format with steps
	var outer struct {
		Result struct {
			Steps map[string]json.RawMessage `json:"steps"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &outer); err != nil {
		return nil
	}

	// Try summary-step.output.final_references first (aggregated list).
	if summaryRaw, ok := outer.Result.Steps["summary-step"]; ok {
		var summary struct {
			Output struct {
				FinalReferences []refJSON `json:"final_references"`
			} `json:"output"`
		}
		if json.Unmarshal(summaryRaw, &summary) == nil && len(summary.Output.FinalReferences) > 0 {
			return deduplicateRefs(summary.Output.FinalReferences)
		}
	}

	// Fallback: collect references from each query-*-step.
	var allRaw []refJSON
	for name, raw := range outer.Result.Steps {
		if !strings.HasPrefix(name, "query-") {
			continue
		}
		var step struct {
			Output struct {
				References []refJSON `json:"references"`
			} `json:"output"`
		}
		if json.Unmarshal(raw, &step) == nil {
			allRaw = append(allRaw, step.Output.References...)
		}
	}
	return deduplicateRefs(allRaw)
}

// refJSON is the upstream reference shape returned by the Mastra workflow.
type refJSON struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	PostID        string `json:"post_id"`
	Title         string `json:"title"`
	Text          string `json:"text"`
	URL           string `json:"url"`
	ProductNameZH string `json:"product_name_zh"`
	ProductNameEN string `json:"product_name_en"`
}

// deduplicateRefs converts raw upstream references to models.Reference,
// deduplicating by URL (or by ID for URL-less entries like package_insert).
func deduplicateRefs(raw []refJSON) []models.Reference {
	seen := make(map[string]bool)
	var refs []models.Reference
	for _, r := range raw {
		// Determine a dedup key — prefer URL, fall back to ID/post_id.
		key := r.URL
		if key == "" {
			key = r.ID
			if key == "" {
				key = r.PostID
			}
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true

		id := r.ID
		if id == "" {
			id = r.PostID
		}

		ref := models.Reference{
			Type:          r.Type,
			ID:            id,
			Title:         r.Title,
			Content:       r.Text,
			URL:           r.URL,
			ProductNameZH: r.ProductNameZH,
			ProductNameEN: r.ProductNameEN,
		}
		if ref.Type == "" {
			ref.Type = "post"
		}
		refs = append(refs, ref)
	}
	return refs
}

// extractHasResults extracts the hasResults boolean from a tool invocation result.
func extractHasResults(result interface{}) *bool {
	if result == nil {
		return nil
	}

	data, err := json.Marshal(result)
	if err != nil {
		return nil
	}

	// Try new format first - direct hasResults in result.result
	var newFormat struct {
		Result struct {
			HasResults bool `json:"hasResults"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &newFormat); err == nil {
		// Only return if hasResults field actually exists in the JSON
		var checkExists map[string]interface{}
		if json.Unmarshal(data, &checkExists) == nil {
			if result, ok := checkExists["result"].(map[string]interface{}); ok {
				if _, exists := result["hasResults"]; exists {
					return &newFormat.Result.HasResults
				}
			}
		}
	}

	// Try direct hasResults in result
	var directFormat struct {
		HasResults bool `json:"hasResults"`
	}
	if err := json.Unmarshal(data, &directFormat); err == nil {
		// Only return if hasResults field actually exists in the JSON
		var checkExists map[string]interface{}
		if json.Unmarshal(data, &checkExists) == nil {
			if _, exists := checkExists["hasResults"]; exists {
				return &directFormat.HasResults
			}
		}
	}

	// Could add fallback to old format if needed, but for now just new format
	return nil
}

// PauseStream cancels an in-flight stream for the given thread. It runs
// two independent steps so a failure in one does not block the other:
//
//  1. Local cancel — releases the BFF's scanner loop and frees resources
//     held by doUpstreamStream. Without this, the BFF would keep reading
//     until the upstream connection naturally closes.
//
//  2. Remote stop — calls Mastra's /custom/api/chat/stop endpoint, which
//     fires the AbortController inside the Mastra process and actually
//     terminates the running agent.stream(). This is the only step that
//     stops the LLM and prevents partial messages from being persisted to
//     memory, because Cloud Run + HTTP/1.1 will not propagate the local
//     TCP close to the Mastra container.
//
// Returns ErrorNotFound only if BOTH the local map and the upstream
// registry had no matching stream — i.e. the stream genuinely does not
// exist (already finished, never started, or wrong identifiers).
func (svc *agentService) PauseStream(ctx context.Context, threadID, userID string) error {
	// Step 1: local cancel.
	localCancelled := svc.cancelLocalStream(threadID)

	// Step 2: remote stop. Use a fresh bounded context so an already-
	// expired parent ctx (e.g. the inbound request was already torn down)
	// does not prevent us from reaching the upstream stop endpoint.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	remoteOk, remoteErr := svc.callRemoteStop(stopCtx, threadID, userID)

	if remoteErr != nil {
		logging.Errorw(ctx, "remote stop failed",
			"thread_id", threadID,
			"user_id", userID,
			"local_cancelled", localCancelled,
			"error", remoteErr)
	}

	// If neither side knew about this stream, surface a not-found so the
	// caller can decide how to handle it (e.g. tell the user the stream
	// was already finished).
	if !localCancelled && !remoteOk && remoteErr == nil {
		logging.Infow(ctx, "no active stream to pause",
			"thread_id", threadID,
			"user_id", userID,
			"active_streams_count", len(svc.activeStreams))
		return e.Wrap(e.ErrorNotFound, "no active stream found for thread")
	}

	return nil
}

// cancelLocalStream cancels the local request context for a stream and
// removes the entry from the active streams map. Returns true if a
// matching entry was found and cancelled.
func (svc *agentService) cancelLocalStream(threadID string) bool {
	svc.streamMutex.Lock()
	defer svc.streamMutex.Unlock()

	cancel, exists := svc.activeStreams[threadID]
	if !exists {
		return false
	}
	cancel()
	delete(svc.activeStreams, threadID)
	return true
}
