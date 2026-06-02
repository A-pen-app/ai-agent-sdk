package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	e "github.com/A-pen-app/errors"
	"github.com/A-pen-app/ai-agent-sdk/models"
	"github.com/A-pen-app/ai-agent-sdk/store"
	"github.com/A-pen-app/logging"
	"github.com/google/uuid"
)

// shareService implements the Share interface: share-link creation, reading
// shared messages, forking a shared thread, and rotating a link's short code.
type shareService struct {
	s              store.Agent
	agentStreamURL string
	httpClient     *http.Client
}

// NewShare creates a new Share service.
func NewShare(s store.Agent, agentStreamURL string, httpClient *http.Client) Share {
	return &shareService{
		s:              s,
		agentStreamURL: agentStreamURL,
		httpClient:     httpClient,
	}
}

func (svc *shareService) CreateShareLink(ctx context.Context, threadID, userID string) (*models.ShareLink, error) {
	// Verify thread ownership
	if _, err := svc.s.GetThread(ctx, threadID, userID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	shareLink := &models.ShareLink{
		ID:          uuid.New().String(),
		Type:        models.ShareLinkTypeAIThread,
		ReferenceID: threadID,
		UserID:      userID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := svc.s.CreateShareLink(ctx, shareLink); err != nil {
		return nil, err
	}

	return shareLink, nil
}

func (svc *shareService) GetShareLink(ctx context.Context, id string) (*models.ShareLink, error) {
	link, err := svc.s.GetShareLink(ctx, id)
	if err != nil {
		return nil, err
	}
	return link, nil
}

func (svc *shareService) ListSharedMessages(ctx context.Context, id, cursor string, count int) (*models.SharedMessageListResponse, error) {
	link, err := svc.s.GetShareLink(ctx, id)
	if err != nil {
		return nil, err
	}
	if link.DeletedAt != nil {
		return nil, e.ErrorNotFound
	}

	rows, err := svc.s.ListSharedMessages(ctx, link.ReferenceID, link.CreatedAt, cursor, count)
	if err != nil {
		return nil, err
	}

	hasMore := len(rows) > count
	if hasMore {
		rows = rows[:count]
	}

	data := make([]models.SharedMessageResponse, len(rows))
	for i, row := range rows {
		content := extractTextContent(row.Content)
		data[i] = models.SharedMessageResponse{
			ID:        row.ID,
			Role:      row.Role,
			Content:   content,
			CreatedAt: row.CreatedAt,
		}
	}

	var next *string
	if hasMore && len(data) > 0 {
		last := data[len(data)-1].ID
		next = &last
	}

	return &models.SharedMessageListResponse{
		Data:    data,
		HasMore: hasMore,
		Next:    next,
	}, nil
}

func (svc *shareService) ForkThread(ctx context.Context, id, newOwnerID string) (*models.ForkThreadResponse, error) {
	link, err := svc.s.GetShareLink(ctx, id)
	if err != nil {
		return nil, err
	}
	if link.DeletedAt != nil {
		return nil, e.ErrorNotFound
	}

	forkURL := svc.agentStreamURL + "/custom/api/thread/fork"
	body := map[string]string{
		"sourceThreadId": link.ReferenceID,
		"endDate":        link.CreatedAt.Format(time.RFC3339Nano),
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal fork request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", forkURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create fork request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-user-id", newOwnerID)

	resp, err := svc.httpClient.Do(req)
	if err != nil {
		logging.Errorw(ctx, "Failed to call fork API",
			"error", err.Error(),
			"share_link_id", id)
		return nil, fmt.Errorf("failed to call fork API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fork API returned status %d", resp.StatusCode)
	}

	var forkResp struct {
		Success            bool        `json:"success"`
		Thread             interface{} `json:"thread"`
		ClonedMessageCount int         `json:"clonedMessageCount"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&forkResp); err != nil {
		return nil, fmt.Errorf("failed to decode fork response: %w", err)
	}

	threadMap, ok := forkResp.Thread.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected fork response thread format")
	}

	threadID, _ := threadMap["id"].(string)
	title, _ := threadMap["title"].(string)

	return &models.ForkThreadResponse{
		ThreadID: threadID,
		Title:    title,
	}, nil
}

func (svc *shareService) UpdateShareLinkShortCode(ctx context.Context, id, shortCode string) error {
	return svc.s.UpdateShareLinkShortCode(ctx, id, shortCode)
}
