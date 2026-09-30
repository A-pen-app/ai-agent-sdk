package store

import (
	"context"
	"fmt"

	e "github.com/A-pen-app/errors"
	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/logging"
	"github.com/jmoiron/sqlx"
)

type agentStore struct {
	// Share persistence is implemented by an embedded shareStore so the
	// Agent interface (which embeds Share) is satisfied through promotion.
	*shareStore
	db *sqlx.DB
}

// NewAgent creates a new Agent store backed by sqlx. schemaName is the
// schema of the product's Mastra memory: "public" for A-Pen,
// "windoc_mastra" for WinDoc.
func NewAgent(db *sqlx.DB, schemaName string) Agent {
	return &agentStore{
		shareStore: &shareStore{db: db, schema: newSchema(schemaName)},
		db:         db,
	}
}

func (s *agentStore) ListThreads(ctx context.Context, userID, cursor string, count int) ([]models.ThreadWithPin, error) {
	query := `
		WITH threads AS (
			SELECT
				t.id,
				t.title,
				COALESCE(p.is_deleted = false, false) AS is_pinned,
				t."updatedAt"
			FROM {schema}.mastra_threads t
			LEFT JOIN {schema}.thread_pin p ON p.thread_id = t.id AND p.user_id = $1
			WHERE t."resourceId" = $1
			AND t."deletedAt" IS NULL
		)
		SELECT id, title, is_pinned
		FROM threads
	`
	args := []interface{}{userID}
	argIdx := 2

	if cursor != "" {
		query += fmt.Sprintf(`
		WHERE (is_pinned, "updatedAt", id) < (
			SELECT is_pinned, "updatedAt", id FROM threads WHERE id = $%d
		)
		`, argIdx)
		args = append(args, cursor)
		argIdx++
	}

	query += fmt.Sprintf(`
		ORDER BY is_pinned DESC, "updatedAt" DESC, id DESC
		LIMIT $%d
	`, argIdx)
	args = append(args, count+1)

	var rows []models.ThreadWithPin
	if err := s.db.Select(&rows, s.schema.sql(query), args...); err != nil {
		logging.Errorw(ctx, "Failed to list threads",
			"user_id", userID,
			"error", err.Error())
		return nil, err
	}
	return rows, nil
}

func (s *agentStore) SearchThreads(ctx context.Context, userID, keyword, cursor string, count int) ([]models.ThreadWithPin, error) {
	query := `
		WITH threads AS (
			SELECT
				t.id,
				t.title,
				COALESCE(p.is_deleted = false, false) AS is_pinned,
				t."updatedAt"
			FROM {schema}.mastra_threads t
			LEFT JOIN {schema}.thread_pin p ON p.thread_id = t.id AND p.user_id = $1
			WHERE t."resourceId" = $1
			AND t."deletedAt" IS NULL
			AND t.title ILIKE '%' || $2 || '%'
		)
		SELECT id, title, is_pinned
		FROM threads
	`
	args := []interface{}{userID, keyword}
	argIdx := 3

	if cursor != "" {
		query += fmt.Sprintf(`
		WHERE (is_pinned, "updatedAt", id) < (
			SELECT is_pinned, "updatedAt", id FROM threads WHERE id = $%d
		)
		`, argIdx)
		args = append(args, cursor)
		argIdx++
	}

	query += fmt.Sprintf(`
		ORDER BY is_pinned DESC, "updatedAt" DESC, id DESC
		LIMIT $%d
	`, argIdx)
	args = append(args, count+1)

	var rows []models.ThreadWithPin
	if err := s.db.Select(&rows, s.schema.sql(query), args...); err != nil {
		logging.Errorw(ctx, "Failed to search threads",
			"user_id", userID,
			"keyword", keyword,
			"error", err.Error())
		return nil, err
	}
	
	return rows, nil
}

func (s *agentStore) CreateThread(ctx context.Context, thread *models.MastraThread) error {
	query := `
		INSERT INTO {schema}.mastra_threads (id, "resourceId", title, metadata, "createdAt", "updatedAt")
		VALUES (:id, :resourceId, :title, :metadata, :createdAt, :updatedAt)
		ON CONFLICT (id) DO UPDATE SET
			title = EXCLUDED.title,
			"updatedAt" = EXCLUDED."updatedAt"
	`
	_, err := s.db.NamedExec(s.schema.sql(query), thread)
	if err != nil {
		logging.Errorw(ctx, "Failed to create thread",
			"thread_id", thread.ID,
			"user_id", thread.ResourceID,
			"title", thread.Title,
			"error", err.Error())
		return err
	}
	
	return nil
}

func (s *agentStore) GetThread(ctx context.Context, threadID, userID string) (*models.ThreadWithPin, error) {
	query := `
		SELECT
			t.id,
			t.title,
			COALESCE(p.is_deleted = false, false) AS is_pinned
		FROM {schema}.mastra_threads t
		LEFT JOIN {schema}.thread_pin p ON p.thread_id = t.id AND p.user_id = $1
		WHERE t.id = $2 AND t."resourceId" = $1 AND t."deletedAt" IS NULL
	`
	var thread models.ThreadWithPin
	err := s.db.Get(&thread, s.schema.sql(query), userID, threadID)
	if err != nil {
		logging.Errorw(ctx, "Thread not found or access denied",
			"thread_id", threadID,
			"user_id", userID,
			"error", err.Error(),
			"db_error", err)
		
		return nil, e.Wrap(e.ErrorNotFound, "thread not found or not owned by user")
	}
	
	return &thread, nil
}

func (s *agentStore) DeleteThread(ctx context.Context, threadID, userID string) error {
	// Soft delete: set deletedAt timestamp instead of removing the row.
	result, err := s.db.Exec(
		s.schema.sql(`UPDATE {schema}.mastra_threads SET "deletedAt" = NOW() WHERE id = $1 AND "resourceId" = $2 AND "deletedAt" IS NULL`),
		threadID, userID,
	)
	if err != nil {
		logging.Errorw(ctx, "Failed to soft delete thread - database error",
			"thread_id", threadID,
			"user_id", userID,
			"error", err.Error())
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		logging.Errorw(ctx, "Failed to get affected rows after soft delete",
			"thread_id", threadID,
			"user_id", userID,
			"error", err.Error())
		return err
	}

	if rowsAffected == 0 {
		logging.Errorw(ctx, "Thread not found for deletion or access denied",
			"thread_id", threadID,
			"user_id", userID,
			"rows_affected", rowsAffected)

		return e.Wrap(e.ErrorNotFound, "thread not found or not owned by user")
	}

	return nil
}

func (s *agentStore) UpdateThread(ctx context.Context, threadID, userID, title string) error {
	query := `UPDATE {schema}.mastra_threads SET title = $1, "updatedAt" = NOW() WHERE id = $2 AND "resourceId" = $3 AND "deletedAt" IS NULL`
	result, err := s.db.Exec(s.schema.sql(query), title, threadID, userID)
	if err != nil {
		logging.Errorw(ctx, "Failed to update thread - database error",
			"thread_id", threadID,
			"user_id", userID,
			"title", title,
			"error", err.Error())
		return err
	}
	
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		logging.Errorw(ctx, "Failed to get affected rows after update",
			"thread_id", threadID,
			"user_id", userID,
			"error", err.Error())
		return err
	}
	
	if rowsAffected == 0 {
		logging.Errorw(ctx, "Thread not found for update or access denied",
			"thread_id", threadID,
			"user_id", userID,
			"title", title,
			"rows_affected", rowsAffected)
		
		return e.Wrap(e.ErrorNotFound, "thread not found or not owned by user")
	}
	
	return nil
}

func (s *agentStore) UpdateThreadPin(ctx context.Context, userID, threadID string, isPinned bool) error {
	if isPinned {
		query := `
			INSERT INTO {schema}.thread_pin (user_id, thread_id, is_deleted, pinned_at)
			VALUES ($1, $2, false, NOW())
			ON CONFLICT (user_id, thread_id)
			DO UPDATE SET is_deleted = false, pinned_at = NOW()
		`
		_, err := s.db.Exec(s.schema.sql(query), userID, threadID)
		if err != nil {
			logging.Errorw(ctx, "Failed to pin thread",
				"user_id", userID,
				"thread_id", threadID,
				"error", err.Error())
			return err
		}
		return nil
	}
	query := `
		UPDATE {schema}.thread_pin SET is_deleted = true
		WHERE user_id = $1 AND thread_id = $2
	`
	_, err := s.db.Exec(s.schema.sql(query), userID, threadID)
	if err != nil {
		logging.Errorw(ctx, "Failed to unpin thread",
			"user_id", userID,
			"thread_id", threadID,
			"error", err.Error())
		return err
	}
	return nil
}

func (s *agentStore) ListMessages(ctx context.Context, threadID, userID, cursor string, count int) ([]models.MessageWithFeedback, error) {
	// Use COALESCE(m."createdAtZ", m."createdAt") everywhere a timestamp is read,
	// ordered or paginated on: "createdAtZ" is the timezone-correct UTC instant,
	// while the legacy "createdAt" (timestamp WITHOUT time zone) holds local
	// wall-clock for older rows and is 8h off. Falling back to the legacy column
	// mirrors Mastra's own `createdAtZ || createdAt` read path.
	query := `
		SELECT
			m.id,
			m.content,
			m.role,
			m.type,
			f.feedback_type,
			COALESCE(m."createdAtZ", m."createdAt") AS "createdAt"
		FROM {schema}.mastra_messages m
		LEFT JOIN {schema}.response_feedback f ON f.message_id = m.id AND f.user_id = $1 AND f.thread_id = $2
		LEFT JOIN {schema}.mastra_threads t ON t.id = m.thread_id
		WHERE m.thread_id = $2
		AND m.role IN ('user', 'assistant')
	` + endedRunMessageFilter
	args := []interface{}{userID, threadID}
	argIdx := 3

	if cursor != "" {
		query += fmt.Sprintf(`
		AND COALESCE(m."createdAtZ", m."createdAt") < (
			SELECT COALESCE("createdAtZ", "createdAt") FROM {schema}.mastra_messages WHERE id = $%d
		)
		`, argIdx)
		args = append(args, cursor)
		argIdx++
	}

	query += fmt.Sprintf(`
		ORDER BY COALESCE(m."createdAtZ", m."createdAt") DESC
		LIMIT $%d
	`, argIdx)
	args = append(args, count+1)

	var rows []models.MessageWithFeedback
	if err := s.db.Select(&rows, s.schema.sql(query), args...); err != nil {
		logging.Errorw(ctx, "Failed to list messages",
			"thread_id", threadID,
			"user_id", userID,
			"error", err.Error())
		return nil, err
	}
	
	return rows, nil
}

func (s *agentStore) UpsertFeedback(ctx context.Context, userID, messageID, feedback string) error {
	query := `
		INSERT INTO {schema}.response_feedback (user_id, thread_id, message_id, feedback_type, created_at, updated_at)
		SELECT $1, m.thread_id, $2, $3, NOW(), NOW()
		FROM {schema}.mastra_messages m
		WHERE m.id = $2
		AND m.role = 'assistant'
		ON CONFLICT (message_id, thread_id, user_id)
		DO UPDATE SET feedback_type = $3, updated_at = NOW()
	`
	result, err := s.db.Exec(s.schema.sql(query), userID, messageID, feedback)
	if err != nil {
		logging.Errorw(ctx, "Failed to upsert feedback - database error",
			"user_id", userID,
			"message_id", messageID,
			"feedback", feedback,
			"error", err.Error())
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		logging.Errorw(ctx, "Failed to get affected rows after feedback upsert",
			"user_id", userID,
			"message_id", messageID,
			"error", err.Error())
		return err
	}

	if rowsAffected == 0 {
		// Message missing OR not an assistant message — both surface as 404 so we
		// don't leak which one it was.
		logging.Errorw(ctx, "Message not found (or not assistant) for feedback upsert",
			"user_id", userID,
			"message_id", messageID,
			"feedback", feedback,
			"rows_affected", rowsAffected)

		return e.Wrap(e.ErrorNotFound, "message not found")
	}

	return nil
}

func (s *agentStore) FindRunningRunID(ctx context.Context, userID, threadID string) (string, error) {
	// At most one row: pen-gpt keeps one active run per (user_id, thread_id).
	query := `
		SELECT run_id::text FROM {schema}.stream_runs
		WHERE user_id = $1 AND thread_id = $2 AND status = 'running'
	`
	var runIDs []string
	if err := s.db.Select(&runIDs, s.schema.sql(query), userID, threadID); err != nil {
		logging.Errorw(ctx, "Failed to find running stream run",
			"thread_id", threadID,
			"user_id", userID,
			"error", err.Error())
		return "", err
	}
	if len(runIDs) != 1 {
		return "", nil
	}
	return runIDs[0], nil
}
