package store

import (
	"context"
	"fmt"
	"time"

	e "github.com/A-pen-app/errors"
	"github.com/A-pen-app/ai-agent-sdk/cursor"
	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/logging"
	"github.com/jmoiron/sqlx"
)

// shareStore handles persistence for share links and their shared messages.
type shareStore struct {
	db     *sqlx.DB
	schema schema
}

// NewShare creates a new Share store backed by sqlx; schemaName as in NewAgent.
func NewShare(db *sqlx.DB, schemaName string) Share {
	return &shareStore{db: db, schema: newSchema(schemaName)}
}

func (s *shareStore) CreateShareLink(ctx context.Context, shareLink *models.ShareLink) error {
	query := `
		INSERT INTO {schema}.share_links (id, type, reference_id, user_id, created_at, updated_at)
		VALUES (:id, :type, :reference_id, :user_id, :created_at, :updated_at)
	`
	if _, err := s.db.NamedExec(s.schema.sql(query), shareLink); err != nil {
		logging.Errorw(ctx, "Failed to create share link",
			"id", shareLink.ID,
			"reference_id", shareLink.ReferenceID,
			"error", err.Error())
		return err
	}
	return nil
}

func (s *shareStore) GetShareLink(ctx context.Context, id string) (*models.ShareLink, error) {
	query := `SELECT id, type, reference_id, user_id, short_code, created_at, deleted_at, updated_at FROM {schema}.share_links WHERE id = $1`
	var link models.ShareLink
	if err := s.db.Get(&link, s.schema.sql(query), id); err != nil {
		logging.Errorw(ctx, "Share link not found",
			"id", id,
			"error", err.Error())
		return nil, e.Wrap(e.ErrorNotFound, "share link not found")
	}
	return &link, nil
}

func (s *shareStore) ListSharedMessages(ctx context.Context, threadID string, endDate time.Time, after *cursor.Position, count int) ([]models.MessageWithFeedback, error) {
	// The share link's created_at ($2) is a timestamptz instant: the snapshot
	// bound compares it with the same messageCreatedAt the page is ordered on.
	query := `
		SELECT
			m.id,
			m.content,
			m.role,
			m.type,
			` + messageCreatedAt + ` AS "createdAt"
		FROM {schema}.mastra_messages m
		LEFT JOIN {schema}.mastra_threads t ON t.id = m.thread_id
		WHERE m.thread_id = $1
		AND m.role IN ('user', 'assistant')
		AND ` + messageCreatedAt + ` <= $2
	` + endedRunMessageFilter
	args := []interface{}{threadID, endDate}
	argIdx := 3

	// Oldest first; id breaks ties between messages written in the same
	// millisecond.
	if after != nil {
		query += fmt.Sprintf(`
		AND (`+messageCreatedAt+`, m.id) > ($%d::timestamptz, $%d)
		`, argIdx, argIdx+1)
		args = append(args, after.CreatedAt, after.ID)
		argIdx += 2
	}

	query += fmt.Sprintf(`
		ORDER BY `+messageCreatedAt+` ASC, m.id ASC
		LIMIT $%d
	`, argIdx)
	args = append(args, count+1)

	var rows []models.MessageWithFeedback
	if err := s.db.Select(&rows, s.schema.sql(query), args...); err != nil {
		logging.Errorw(ctx, "Failed to list shared messages",
			"thread_id", threadID,
			"error", err.Error())
		return nil, err
	}
	return rows, nil
}

func (s *shareStore) UpdateShareLinkShortCode(ctx context.Context, id, shortCode string) error {
	query := `UPDATE {schema}.share_links SET short_code = $1, updated_at = NOW() WHERE id = $2`
	if _, err := s.db.Exec(s.schema.sql(query), shortCode, id); err != nil {
		logging.Errorw(ctx, "Failed to update share link short code",
			"id", id,
			"error", err.Error())
		return err
	}
	return nil
}
