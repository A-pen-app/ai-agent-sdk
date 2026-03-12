package store

import (
	"fmt"

	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/jmoiron/sqlx"
)

type agentStore struct {
	db *sqlx.DB
}

// NewAgent creates a new Agent store backed by sqlx.
func NewAgent(db *sqlx.DB) Agent {
	return &agentStore{db: db}
}

func (s *agentStore) ListThreads(userID, cursor string, count int) ([]models.ThreadWithPin, error) {
	query := `
		WITH threads AS (
			SELECT
				t.id,
				t.title,
				COALESCE(p.is_deleted = false, false) AS is_pinned,
				t."updatedAt"
			FROM mastra_threads t
			LEFT JOIN thread_pin p ON p.thread_id = t.id AND p.user_id = $1
			WHERE t."resourceId" = $1
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
	if err := s.db.Select(&rows, query, args...); err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *agentStore) SearchThreads(userID, keyword, cursor string, count int) ([]models.ThreadWithPin, error) {
	query := `
		WITH threads AS (
			SELECT
				t.id,
				t.title,
				COALESCE(p.is_deleted = false, false) AS is_pinned,
				t."updatedAt"
			FROM mastra_threads t
			LEFT JOIN thread_pin p ON p.thread_id = t.id AND p.user_id = $1
			WHERE t."resourceId" = $1
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
	if err := s.db.Select(&rows, query, args...); err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *agentStore) CreateThread(thread *models.MastraThread) error {
	query := `
		INSERT INTO mastra_threads (id, "resourceId", title, metadata, "createdAt", "updatedAt")
		VALUES (:id, :resourceId, :title, :metadata, :createdAt, :updatedAt)
		ON CONFLICT (id) DO UPDATE SET
			title = EXCLUDED.title,
			"updatedAt" = EXCLUDED."updatedAt"
	`
	_, err := s.db.NamedExec(query, thread)
	return err
}

func (s *agentStore) GetThread(threadID, userID string) (*models.ThreadWithPin, error) {
	query := `
		SELECT
			t.id,
			t.title,
			COALESCE(p.is_deleted = false, false) AS is_pinned
		FROM mastra_threads t
		LEFT JOIN thread_pin p ON p.thread_id = t.id AND p.user_id = $1
		WHERE t.id = $2 AND t."resourceId" = $1
	`
	var thread models.ThreadWithPin
	err := s.db.Get(&thread, query, userID, threadID)
	if err != nil {
		return nil, fmt.Errorf("thread not found or not owned by user")
	}
	return &thread, nil
}

func (s *agentStore) DeleteThread(threadID, userID string) error {
	// Delete the thread (only if owned by user).
	result, err := s.db.Exec(`DELETE FROM mastra_threads WHERE id = $1 AND "resourceId" = $2`, threadID, userID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("thread not found or not owned by user")
	}
	// Delete related records.
	_, _ = s.db.Exec(`DELETE FROM mastra_messages WHERE thread_id = $1`, threadID)
	_, _ = s.db.Exec(`DELETE FROM thread_pin WHERE thread_id = $1 AND user_id = $2`, threadID, userID)
	_, _ = s.db.Exec(`DELETE FROM response_feedback WHERE thread_id = $1 AND user_id = $2`, threadID, userID)
	return nil
}

func (s *agentStore) UpdateThread(threadID, userID, title string) error {
	query := `UPDATE mastra_threads SET title = $1, "updatedAt" = NOW() WHERE id = $2 AND "resourceId" = $3`
	result, err := s.db.Exec(query, title, threadID, userID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("thread not found or not owned by user")
	}
	return nil
}

func (s *agentStore) UpdateThreadPin(userID, threadID string, isPinned bool) error {
	if isPinned {
		query := `
			INSERT INTO thread_pin (user_id, thread_id, is_deleted, pinned_at)
			VALUES ($1, $2, false, NOW())
			ON CONFLICT (user_id, thread_id)
			DO UPDATE SET is_deleted = false, pinned_at = NOW()
		`
		_, err := s.db.Exec(query, userID, threadID)
		return err
	}
	query := `
		UPDATE thread_pin SET is_deleted = true
		WHERE user_id = $1 AND thread_id = $2
	`
	_, err := s.db.Exec(query, userID, threadID)
	return err
}

func (s *agentStore) ListMessages(threadID, userID, cursor string, count int) ([]models.MessageWithFeedback, error) {
	query := `
		SELECT
			m.id,
			m.content,
			m.role,
			m.type,
			f.feedback_type,
			m."createdAt"
		FROM mastra_messages m
		LEFT JOIN response_feedback f ON f.message_id = m.id AND f.user_id = $1 AND f.thread_id = $2
		WHERE m.thread_id = $2
		AND m.role IN ('user', 'assistant')
	`
	args := []interface{}{userID, threadID}
	argIdx := 3

	if cursor != "" {
		query += fmt.Sprintf(`
		AND m."createdAt" > (SELECT "createdAt" FROM mastra_messages WHERE id = $%d)
		`, argIdx)
		args = append(args, cursor)
		argIdx++
	}

	query += fmt.Sprintf(`
		ORDER BY m."createdAt" ASC
		LIMIT $%d
	`, argIdx)
	args = append(args, count+1)

	var rows []models.MessageWithFeedback
	if err := s.db.Select(&rows, query, args...); err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *agentStore) UpsertFeedback(userID, messageID, feedback string) error {
	query := `
		INSERT INTO response_feedback (user_id, thread_id, message_id, feedback_type, created_at, updated_at)
		SELECT $1, m.thread_id, $2, $3, NOW(), NOW()
		FROM mastra_messages m
		WHERE m.id = $2
		ON CONFLICT (message_id, thread_id, user_id)
		DO UPDATE SET feedback_type = $3, updated_at = NOW()
	`
	result, err := s.db.Exec(query, userID, messageID, feedback)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		return fmt.Errorf("message not found")
	}

	return nil
}
