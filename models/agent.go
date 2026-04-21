package models

import "time"

// --- DB row structs ---

// MastraThread maps to the mastra_threads table (camelCase columns).
type MastraThread struct {
	ID         string     `db:"id" json:"id"`
	ResourceID string     `db:"resourceId" json:"resourceId"`
	Title      string     `db:"title" json:"title"`
	Metadata   *string    `db:"metadata" json:"metadata,omitempty"`
	CreatedAt  time.Time  `db:"createdAt" json:"createdAt"`
	UpdatedAt  time.Time  `db:"updatedAt" json:"updatedAt"`
	DeletedAt  *time.Time `db:"deletedAt" json:"deletedAt,omitempty"`
}

// MastraMessage maps to the mastra_messages table (camelCase columns).
type MastraMessage struct {
	ID        string    `db:"id" json:"id"`
	ThreadID  string    `db:"thread_id" json:"thread_id"`
	Content   string    `db:"content" json:"content"`
	Role      string    `db:"role" json:"role"`
	Type      string    `db:"type" json:"type"`
	CreatedAt time.Time `db:"createdAt" json:"createdAt"`
}

// ThreadPin maps to the thread_pin table.
type ThreadPin struct {
	UserID    string     `db:"user_id" json:"user_id"`
	ThreadID  string     `db:"thread_id" json:"thread_id"`
	IsDeleted bool       `db:"is_deleted" json:"is_deleted"`
	PinnedAt  *time.Time `db:"pinned_at" json:"pinned_at,omitempty"`
}

// ResponseFeedback maps to the response_feedback table.
type ResponseFeedback struct {
	ThreadID     string     `db:"thread_id" json:"thread_id"`
	MessageID    string     `db:"message_id" json:"message_id"`
	UserID       string     `db:"user_id" json:"user_id"`
	FeedbackType *string    `db:"feedback_type" json:"feedback_type,omitempty"`
	CreatedAt    *time.Time `db:"created_at" json:"created_at,omitempty"`
	UpdatedAt    *time.Time `db:"updated_at" json:"updated_at,omitempty"`
}

// --- Joined query structs ---

// ThreadWithPin is the result of joining mastra_threads with thread_pin.
type ThreadWithPin struct {
	ID       string `db:"id" json:"id"`
	Title    string `db:"title" json:"title"`
	IsPinned bool   `db:"is_pinned" json:"is_pinned"`
}

// MessageWithFeedback is the result of joining mastra_messages with response_feedback.
type MessageWithFeedback struct {
	ID        string    `db:"id" json:"id"`
	Content   string    `db:"content" json:"content"`
	Role      string    `db:"role" json:"role"`
	Type      string    `db:"type" json:"type"`
	Feedback  *string   `db:"feedback_type" json:"feedback,omitempty"`
	CreatedAt time.Time `db:"createdAt" json:"createdAt"`
}

// --- API response structs ---

// ThreadResponse is a single thread in the API response.
type ThreadResponse struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	IsPinned bool   `json:"is_pinned"`
}

// ThreadListResponse is the paginated thread list response.
type ThreadListResponse struct {
	Data []ThreadResponse `json:"data"`
	Next *string          `json:"next"`
}

// WorkflowStep represents a tool invocation step shown to the user.
type WorkflowStep struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// Reference represents a source reference from tool results.
type Reference struct {
	Type          string      `json:"type" example:"post"`                                                                    // Type of reference (e.g., "post")
	ID            string      `json:"id" example:"123e4567-e89b-12d3-a456-426614174000"`                                       // Unique identifier of the referenced item
	Title         string      `json:"title,omitempty" example:"Example Post Title"`                                           // Title of the referenced item
	Content       string      `json:"content,omitempty" example:"This is the content of the referenced post..."`             // Content or excerpt of the referenced item
	URL           string      `json:"url,omitempty" example:"apen://posts/123e4567-e89b-12d3-a456-426614174000"`            // URL to access the referenced item
	ProductNameZH string      `json:"product_name_zh,omitempty" example:"产品名称"`                                           // Product name in Chinese (if applicable)
	ProductNameEN string      `json:"product_name_en,omitempty" example:"Product Name"`                                     // Product name in English (if applicable)
	Post          interface{} `json:"post,omitempty" swaggertype:"object"`  // Complete post data when reference type is "post"
}

// ToolResult represents the result structure in new format tool invocations
type ToolResult struct {
	Result *WorkflowResult `json:"result,omitempty"`
	RunID  string          `json:"runId,omitempty"`
}

// WorkflowResult represents the workflow execution result in new format
type WorkflowResult struct {
	Summary         string      `json:"summary,omitempty"`
	FinalReferences []Reference `json:"final_references,omitempty"`
	HasResults      bool        `json:"hasResults,omitempty"`
}


// MessageResponse is a single message in the API response.
type MessageResponse struct {
	ID            string         `json:"id"`
	Role          string         `json:"role"`
	Content       string         `json:"content"`
	Feedback      *string        `json:"feedback,omitempty"`
	WorkflowSteps []WorkflowStep `json:"workflow_steps,omitempty"`
	References    []Reference    `json:"references,omitempty"`
	HasResults    *bool          `json:"has_results,omitempty"`
}

// MessageListResponse is the paginated message list response.
type MessageListResponse struct {
	Data []MessageResponse `json:"data"`
	Next *string           `json:"next"`
}

// SuccessResponse is a generic success response.
type SuccessResponse struct {
	Success bool `json:"success"`
}

// --- API request structs ---

// CreateThreadRequest is the request body for creating a thread.
type CreateThreadRequest struct {
	Query string `json:"query" binding:"required"`
}

// UpdateThreadRequest is the request body for updating a thread (title and/or pin status).
type UpdateThreadRequest struct {
	Title    *string `json:"title,omitempty"`
	IsPinned *bool   `json:"is_pinned,omitempty"`
}

// StreamRequest is the request body for the stream endpoint.
type StreamRequest struct {
	ThreadID string `json:"thread_id"` // ThreadID now comes from URL path parameter, not required in JSON
	Query    string `json:"query" binding:"required"`
}

// --- SSE stream types ---

// StreamEventType defines the event types sent to the frontend via SSE.
type StreamEventType string

const (
	StreamEventStart        StreamEventType = "start"
	StreamEventWorkflowStep StreamEventType = "workflow_step"
	StreamEventTextDelta    StreamEventType = "text_delta"
	StreamEventReferences   StreamEventType = "references"
	StreamEventFinish       StreamEventType = "finish"
	StreamEventDone         StreamEventType = "done"
	StreamEventError        StreamEventType = "error"
)

// StreamEnvelope is the JSON structure sent as SSE data.
type StreamEnvelope struct {
	Event StreamEventType `json:"event"`
	Data  interface{}     `json:"data"`
}

// TextDeltaData is the payload for text_delta events.
type TextDeltaData struct {
	Text string `json:"text"`
}

// StreamErrorData is the payload for error events.
type StreamErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// FeedbackRequest is the request body for submitting feedback.
type FeedbackRequest struct {
	Feedback string `json:"feedback" binding:"required,oneof=like unlike none"`
}

// --- V2 content parsing structs (internal) ---

// MastraContentV2 represents the top-level V2 content structure.
// Format: {"format":2,"parts":[...]}
type MastraContentV2 struct {
	Format int            `json:"format"`
	Parts  []MastraV2Part `json:"parts"`
}

// MastraV2Part represents a single part within V2 content.
type MastraV2Part struct {
	Type           string          `json:"type"`
	Text           string          `json:"text,omitempty"`
	ToolInvocation *ToolInvocation `json:"toolInvocation,omitempty"`
}

// ToolInvocation represents a tool call/result nested inside a V2 part.
type ToolInvocation struct {
	State      string      `json:"state"`
	ToolCallID string      `json:"toolCallId"`
	ToolName   string      `json:"toolName"`
	Args       interface{} `json:"args,omitempty"`
	Result     interface{} `json:"result,omitempty"`
}
