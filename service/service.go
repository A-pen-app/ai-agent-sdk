package service

import (
	"context"

	"github.com/A-pen-app/ai-agent-sdk/models"
)

// StreamWriter is a callback that sends an SSE envelope to the client.
type StreamWriter func(envelope *models.StreamEnvelope) error

type Agent interface {
	ListThreads(ctx context.Context, userID, cursor string, count int) (*models.ThreadListResponse, error)
	SearchThreads(ctx context.Context, userID, keyword, cursor string, count int) (*models.ThreadListResponse, error)
	CreateThread(ctx context.Context, userID, query string) (*models.ThreadResponse, error)
	GetThread(ctx context.Context, threadID, userID string) (*models.ThreadResponse, error)
	DeleteThread(ctx context.Context, threadID, userID string) error
	UpdateThread(ctx context.Context, threadID, userID, title string) (*models.ThreadResponse, error)
	UpdateThreadPin(ctx context.Context, userID, threadID string, isPinned bool) error
	ListMessages(ctx context.Context, threadID, userID, cursor string, count int) (*models.MessageListResponse, error)
	UpsertFeedback(ctx context.Context, userID, messageID, feedback string) error
	StreamChat(ctx context.Context, userID string, req *models.StreamRequest, writer StreamWriter) error
	PauseStream(ctx context.Context, threadID, userID string) error
}
