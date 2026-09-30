package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/A-pen-app/ai-agent-sdk/models"
	e "github.com/A-pen-app/errors"
	"github.com/gin-gonic/gin"
)

// shareLinkStore 回一個有效的分享連結，讓 ListSharedMessages 走到 cursor 解析。
type shareLinkStore struct{ fakeStore }

func (shareLinkStore) GetShareLink(context.Context, string) (*models.ShareLink, error) {
	return &models.ShareLink{ID: "link-1", ReferenceID: "t1", CreatedAt: time.Now()}, nil
}

// BFF 把 ListMessages／ListSharedMessages 的錯誤原樣交給 e.Handle：v0.0.17 以前的
// bare id 要回 400 WRONG_PARAMETER，前端才會清掉 next 重讀第一頁。
func TestInvalidCursorAnswers400ThroughHandle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agent := NewAgent(fakeStore{}, "http://unused")
	share := NewShare(shareLinkStore{}, "http://unused", nil)
	router := gin.New()
	router.GET("/threads/:id/messages", e.Handle(func(ctx *gin.Context) error {
		_, err := agent.ListMessages(ctx.Request.Context(), ctx.Param("id"), "u1", ctx.Query("next"), 20)
		return err
	}))
	router.GET("/share/:id/messages", e.Handle(func(ctx *gin.Context) error {
		_, err := share.ListSharedMessages(ctx.Request.Context(), ctx.Param("id"), ctx.Query("next"), 20)
		return err
	}))

	const bareID = "23189f32-53c5-4b69-8704-f61abc604862"
	for _, path := range []string{"/threads/t1/messages", "/share/link-1/messages"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path+"?next="+bareID, nil))
		var body e.HttpError
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body %q: %v", path, w.Body.String(), err)
		}
		if w.Code != http.StatusBadRequest || body.Code != string(e.KeyWrongParams) {
			t.Errorf("%s: got %d %q, want 400 %q", path, w.Code, body.Code, e.KeyWrongParams)
		}
	}
}
