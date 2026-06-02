package store

import (
	"context"
	"fmt"
	"time"

	e "github.com/A-pen-app/errors"
	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/logging"
	"github.com/jmoiron/sqlx"
)

// shareStore handles persistence for share links and their shared messages.
type shareStore struct {
	db *sqlx.DB
}

// NewShare creates a new Share store backed by sqlx.
func NewShare(db *sqlx.DB) Share {
	return &shareStore{db: db}
}

func (s *shareStore) CreateShareLink(ctx context.Context, shareLink *models.ShareLink) error {
	query := `
		INSERT INTO share_links (id, type, reference_id, user_id, created_at, updated_at)
		VALUES (:id, :type, :reference_id, :user_id, :created_at, :updated_at)
	`
	if _, err := s.db.NamedExec(query, shareLink); err != nil {
		logging.Errorw(ctx, "Failed to create share link",
			"id", shareLink.ID,
			"reference_id", shareLink.ReferenceID,
			"error", err.Error())
		return err
	}
	return nil
}

func (s *shareStore) GetShareLink(ctx context.Context, id string) (*models.ShareLink, error) {
	query := `SELECT id, type, reference_id, user_id, short_code, created_at, deleted_at, updated_at FROM share_links WHERE id = $1`
	var link models.ShareLink
	if err := s.db.Get(&link, query, id); err != nil {
		logging.Errorw(ctx, "Share link not found",
			"id", id,
			"error", err.Error())
		return nil, e.Wrap(e.ErrorNotFound, "share link not found")
	}
	return &link, nil
}

func (s *shareStore) ListSharedMessages(ctx context.Context, threadID string, endDate time.Time, cursor string, count int) ([]models.MessageWithFeedback, error) {
	// mastra_messages has two timestamp columns: the legacy "createdAt"
	// (timestamp WITHOUT time zone, which for older rows holds local wall-clock
	// and is 8h off real UTC) and "createdAtZ" (timestamptz, the correct UTC
	// instant). The share link's created_at ($2) is a real timestamptz instant,
	// so we compare against "createdAtZ" to get a timezone-correct snapshot
	// bound regardless of server/session timezone. COALESCE falls back to the
	// legacy column for any row that predates createdAtZ, mirroring Mastra's own
	// `createdAtZ || createdAt` read path.
	query := `
		SELECT
			m.id,
			m.content,
			m.role,
			m.type,
			COALESCE(m."createdAtZ", m."createdAt") AS "createdAt"
		FROM mastra_messages m
		WHERE m.thread_id = $1
		AND m.role IN ('user', 'assistant')
		AND COALESCE(m."createdAtZ", m."createdAt") <= $2
	`
	args := []interface{}{threadID, endDate}
	argIdx := 3

	if cursor != "" {
		query += fmt.Sprintf(`
		AND COALESCE(m."createdAtZ", m."createdAt") > (
			SELECT COALESCE("createdAtZ", "createdAt") FROM mastra_messages WHERE id = $%d
		)
		`, argIdx)
		args = append(args, cursor)
		argIdx++
	}

	query += fmt.Sprintf(`
		ORDER BY COALESCE(m."createdAtZ", m."createdAt") ASC
		LIMIT $%d
	`, argIdx)
	args = append(args, count+1)

	var rows []models.MessageWithFeedback
	if err := s.db.Select(&rows, query, args...); err != nil {
		logging.Errorw(ctx, "Failed to list shared messages",
			"thread_id", threadID,
			"error", err.Error())
		return nil, err
	}
	return rows, nil
}

func (s *shareStore) UpdateShareLinkShortCode(ctx context.Context, id, shortCode string) error {
	query := `UPDATE share_links SET short_code = $1, updated_at = NOW() WHERE id = $2`
	if _, err := s.db.Exec(query, shortCode, id); err != nil {
		logging.Errorw(ctx, "Failed to update share link short code",
			"id", id,
			"error", err.Error())
		return err
	}
	return nil
}
