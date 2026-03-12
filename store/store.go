package store

import "github.com/A-pen-app/ai-agent-sdk/models"

type Agent interface {
	ListThreads(userID, cursor string, count int) ([]models.ThreadWithPin, error)
	SearchThreads(userID, keyword, cursor string, count int) ([]models.ThreadWithPin, error)
	CreateThread(thread *models.MastraThread) error
	GetThread(threadID, userID string) (*models.ThreadWithPin, error)
	DeleteThread(threadID, userID string) error
	UpdateThread(threadID, userID, title string) error
	UpdateThreadPin(userID, threadID string, isPinned bool) error
	ListMessages(threadID, userID, cursor string, count int) ([]models.MessageWithFeedback, error)
	UpsertFeedback(userID, messageID, feedback string) error
}
