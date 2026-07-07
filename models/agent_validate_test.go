package models

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUpdateThreadRequestValidate(t *testing.T) {
	parse := func(body string) *UpdateThreadRequest {
		var r UpdateThreadRequest
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		return &r
	}

	// 欄位未帶 → no-op，合法
	if err := parse(`{}`).Validate(); err != nil {
		t.Fatalf("empty patch should be valid, got %v", err)
	}

	// null / 空字串 / 超長 → ErrValidation
	for _, body := range []string{
		`{"title":null}`,
		`{"title":"   "}`,
		`{"title":"` + strings.Repeat("題", MaxThreadTitleLen+1) + `"}`,
		`{"is_pinned":null}`,
	} {
		if err := parse(body).Validate(); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", body, err)
		}
	}

	// 合法值 → 通過且 title 被 trim
	r := parse(`{"title":"  台北內科諮詢  ","is_pinned":true}`)
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if *r.Title.Value != "台北內科諮詢" {
		t.Fatalf("title not trimmed: %q", *r.Title.Value)
	}

	// 重複 key、後者為 null → 不得殘留前值，仍應被 Validate 擋下
	if err := parse(`{"title":"x","title":null}`).Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("duplicated key with trailing null should fail validation, got %v", err)
	}
}
