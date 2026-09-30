package store

import (
	"context"
	"time"

	"github.com/A-pen-app/ai-agent-sdk/cursor"
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
	ListMessages(ctx context.Context, threadID, userID string, after *cursor.Position, count int) ([]models.MessageWithFeedback, error)
	UpsertFeedback(ctx context.Context, userID, messageID, feedback string) error
	// FindRunningRunID returns the run id of the thread's `running` run in
	// pen-gpt's stream_runs, or "" when the thread has none.
	FindRunningRunID(ctx context.Context, userID, threadID string) (string, error)

	Share
}

// Share handles persistence for share links and their shared messages.
type Share interface {
	CreateShareLink(ctx context.Context, shareLink *models.ShareLink) error
	GetShareLink(ctx context.Context, id string) (*models.ShareLink, error)
	ListSharedMessages(ctx context.Context, threadID string, endDate time.Time, after *cursor.Position, count int) ([]models.MessageWithFeedback, error)
	UpdateShareLinkShortCode(ctx context.Context, id, shortCode string) error
}
