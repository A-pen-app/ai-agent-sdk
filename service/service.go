package service

import (
	"context"

	"github.com/A-pen-app/ai-agent-sdk/models"
)

// StreamWriter is a callback that sends an SSE envelope to the client.
type StreamWriter func(envelope *models.StreamEnvelope) error

type Agent interface {
	ListThreads(userID, cursor string, count int) (*models.ThreadListResponse, error)
	SearchThreads(userID, keyword, cursor string, count int) (*models.ThreadListResponse, error)
	CreateThread(userID, query string) (*models.ThreadResponse, error)
	GetThread(threadID, userID string) (*models.ThreadResponse, error)
	DeleteThread(threadID, userID string) error
	UpdateThread(threadID, userID, title string) (*models.ThreadResponse, error)
	UpdateThreadPin(userID, threadID string, isPinned bool) error
	ListMessages(threadID, userID, cursor string, count int) (*models.MessageListResponse, error)
	UpsertFeedback(userID, messageID, feedback string) error
	StreamChat(ctx context.Context, userID string, req *models.StreamRequest, writer StreamWriter) error
	PauseStream(threadID, userID string) error
}
