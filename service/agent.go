package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

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

func (svc *agentService) ListThreads(userID, cursor string, count int) (*models.ThreadListResponse, error) {
	rows, err := svc.s.ListThreads(userID, cursor, count)
	if err != nil {
		return nil, err
	}
	return buildThreadListResponse(rows, count), nil
}

func (svc *agentService) SearchThreads(userID, keyword, cursor string, count int) (*models.ThreadListResponse, error) {
	rows, err := svc.s.SearchThreads(userID, keyword, cursor, count)
	if err != nil {
		return nil, err
	}
	return buildThreadListResponse(rows, count), nil
}

func (svc *agentService) CreateThread(userID, query string) (*models.ThreadResponse, error) {
	now := time.Now().UTC()
	thread := &models.MastraThread{
		ID:         uuid.New().String(),
		ResourceID: userID,
		Title:      query,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := svc.s.CreateThread(thread); err != nil {
		return nil, err
	}
	return &models.ThreadResponse{
		ID:       thread.ID,
		Title:    thread.Title,
		IsPinned: false,
	}, nil
}

func (svc *agentService) GetThread(threadID, userID string) (*models.ThreadResponse, error) {
	threadWithPin, err := svc.s.GetThread(threadID, userID)
	if err != nil {
		return nil, err
	}
	return &models.ThreadResponse{
		ID:       threadWithPin.ID,
		Title:    threadWithPin.Title,
		IsPinned: threadWithPin.IsPinned,
	}, nil
}

func (svc *agentService) DeleteThread(threadID, userID string) error {
	return svc.s.DeleteThread(threadID, userID)
}

func (svc *agentService) UpdateThread(threadID, userID, title string) (*models.ThreadResponse, error) {
	if err := svc.s.UpdateThread(threadID, userID, title); err != nil {
		return nil, err
	}
	return &models.ThreadResponse{
		ID:    threadID,
		Title: title,
	}, nil
}

func (svc *agentService) UpdateThreadPin(userID, threadID string, isPinned bool) error {
	return svc.s.UpdateThreadPin(userID, threadID, isPinned)
}

func (svc *agentService) ListMessages(threadID, userID, cursor string, count int) (*models.MessageListResponse, error) {
	rows, err := svc.s.ListMessages(threadID, userID, cursor, count)
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
	return resp, nil
}

func (svc *agentService) UpsertFeedback(userID, messageID, feedback string) error {
	return svc.s.UpsertFeedback(userID, messageID, feedback)
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
				innerSteps := extractWorkflowSteps(ti.Args, ti.Result)
				if len(innerSteps) > 0 {
					steps = append(steps, innerSteps...)
				}
				refs = append(refs, extractReferences(ti.Result)...)
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
	}
	if err := json.Unmarshal(argsData, &wfArgs); err != nil {
		return nil
	}

	// Build a lookup for execute flags.
	execFlags := map[string]bool{
		"executeMedical":       wfArgs.ExecuteMedical,
		"executeDrug":          wfArgs.ExecuteDrug,
		"executeInsite":        wfArgs.ExecuteInsite,
		"executeLaw":           wfArgs.ExecuteLaw,
		"executeLecture":       wfArgs.ExecuteLecture,
		"executeNhiRegulation": wfArgs.ExecuteNhiRegulation,
		"executeFinance":       wfArgs.ExecuteFinance,
	}

	// If result is provided, parse it to verify which steps exist.
	var resultSteps map[string]json.RawMessage
	if result != nil {
		resultData, err := json.Marshal(result)
		if err == nil {
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

	var steps []models.WorkflowStep
	for _, def := range workflowStepDefs {
		// When result is available, skip steps that don't exist in it.
		if resultSteps != nil {
			if _, ok := resultSteps[def.StepName]; !ok {
				continue
			}
		}
		// Skip steps whose execute flag is false.
		if def.ExecuteFlag != "" && !execFlags[def.ExecuteFlag] {
			continue
		}
		displayName := translateStepName(def.StepName, wfArgs.Query)
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

// PauseStream cancels the active stream for the given thread.
func (svc *agentService) PauseStream(threadID, userID string) error {
	svc.streamMutex.Lock()
	defer svc.streamMutex.Unlock()

	cancel, exists := svc.activeStreams[threadID]
	if !exists {
		return fmt.Errorf("no active stream found for thread %s", threadID)
	}

	// Cancel the context to stop the stream
	cancel()

	// Remove from active streams map
	delete(svc.activeStreams, threadID)

	return nil
}
