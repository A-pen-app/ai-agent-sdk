package store

import (
	"context"
	"github.com/A-pen-app/ai-agent-sdk/models"
)

type Agent interface {
	ListThreads(ctx context.Context, userID, cursor string, count int) ([]models.ThreadWithPin, error)
	SearchThreads(ctx context.Context, userID, keyword, cursor string, count int) ([]models.ThreadWithPin, error)
	CreateThread(ctx context.Context, thread *models.MastraThread) error
	GetThread(ctx context.Context, threadID, userID string) (*models.ThreadWithPin, error)
	DeleteThread(ctx context.Context, threadID, userID string) error
	UpdateThread(ctx context.Context, threadID, userID, title string) error
	UpdateThreadPin(ctx context.Context, userID, threadID string, isPinned bool) error
	ListMessages(ctx context.Context, threadID, userID, cursor string, count int) ([]models.MessageWithFeedback, error)
	UpsertFeedback(ctx context.Context, userID, messageID, feedback string) error
}
