package service

import (
	"context"

	"github.com/A-pen-app/ai-agent-sdk/cursor"
	"github.com/A-pen-app/ai-agent-sdk/models"
)

// ErrInvalidCursor is returned by ListMessages and ListSharedMessages for a
// next token they cannot read, including a pre-v0.0.18 cursor (a bare message
// id). It is cursor.ErrInvalid itself; answer it with 400 so the client
// reloads the first page.
var ErrInvalidCursor = cursor.ErrInvalid

// nextCursor returns the token after the last of rows, or nil when there is
// no further page. rows must be in the order the store returned them.
func nextCursor(rows []models.MessageWithFeedback, hasMore bool) (*string, error) {
	if !hasMore || len(rows) == 0 {
		return nil, nil
	}
	last := rows[len(rows)-1]
	token, err := cursor.Encode(cursor.Position{CreatedAt: last.CreatedAt, ID: last.ID})
	if err != nil {
		return nil, err
	}
	return &token, nil
}

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

	Share
}

// Share exposes share-link operations: creating links, reading shared
// messages, forking a shared thread, and rotating a link's short code.
type Share interface {
	CreateShareLink(ctx context.Context, threadID, userID string) (*models.ShareLink, error)
	GetShareLink(ctx context.Context, id string) (*models.ShareLink, error)
	ListSharedMessages(ctx context.Context, id, cursor string, count int) (*models.SharedMessageListResponse, error)
	ForkThread(ctx context.Context, id, newOwnerID string) (*models.ForkThreadResponse, error)
	UpdateShareLinkShortCode(ctx context.Context, id, shortCode string) error
}
