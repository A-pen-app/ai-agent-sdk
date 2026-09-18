package service

import (
	"encoding/json"
	"testing"

	"github.com/A-pen-app/ai-agent-sdk/models"
)

// 無圖時 content 必須還是字串（upstream body 形狀不能變），有圖時才換成 parts。
func TestUserContent(t *testing.T) {
	if got := userContent(&models.StreamRequest{Query: "附近有皮膚科嗎"}); got != "附近有皮膚科嗎" {
		t.Fatalf("plain query should stay a string, got %#v", got)
	}

	got := userContent(&models.StreamRequest{
		Query:     "這張報告怎麼看",
		ImageURLs: []string{"https://cdn/chat/u/1.jpg", "https://cdn/chat/u/2.jpg"},
	})
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"text":"這張報告怎麼看","type":"text"},` +
		`{"data":"https://cdn/chat/u/1.jpg","mediaType":"image/jpeg","type":"file"},` +
		`{"data":"https://cdn/chat/u/2.jpg","mediaType":"image/jpeg","type":"file"}]`
	if string(b) != want {
		t.Fatalf("parts mismatch:\n got %s\nwant %s", b, want)
	}
}
