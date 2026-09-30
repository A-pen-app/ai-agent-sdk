package cursor

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	e "github.com/A-pen-app/errors"
)

func raw(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestTokenFormat(t *testing.T) {
	p := Position{CreatedAt: time.Date(2026, 9, 27, 9, 52, 47, 883000000, time.UTC), ID: "23189f32-53c5-4b69-8704-f61abc604862"}
	token, err := Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := raw("v1.1790502767883000.23189f32-53c5-4b69-8704-f61abc604862"); token != want {
		t.Fatalf("Encode = %s, want %s", token, want)
	}
	got, err := Decode(token)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(p.CreatedAt) || got.ID != p.ID {
		t.Fatalf("Decode = %+v, want %+v", got, p)
	}
}

func TestRoundTripKeepsMicroseconds(t *testing.T) {
	for _, ts := range []time.Time{
		time.Date(2026, 9, 30, 2, 0, 0, 123456000, time.UTC),
		time.Date(2026, 9, 30, 10, 0, 0, 123456000, time.FixedZone("Asia/Taipei", 8*3600)),
		time.Date(1969, 12, 31, 23, 59, 59, 999999000, time.UTC), // negative micros
	} {
		token, err := Encode(Position{CreatedAt: ts, ID: "m"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(token)
		if err != nil {
			t.Fatal(err)
		}
		if got.CreatedAt.UnixMicro() != ts.UnixMicro() {
			t.Errorf("%v: round trip = %v", ts, got.CreatedAt)
		}
	}
}

func TestEmptyTokenIsFirstPage(t *testing.T) {
	got, err := Decode("")
	if got != nil || err != nil {
		t.Fatalf("Decode(\"\") = %v, %v", got, err)
	}
}

func TestInvalidTokens(t *testing.T) {
	for name, token := range map[string]string{
		"bare message id (pre-v1 cursor)": "23189f32-53c5-4b69-8704-f61abc604862",
		"padded base64":                   base64.URLEncoding.EncodeToString([]byte("v1.1.mm")),
		"standard alphabet":               base64.StdEncoding.EncodeToString([]byte("v1.1.m>?")),
		"not base64":                      "!!!",
		"unknown version":                 raw("v2.1.m"),
		"no version":                      raw("1.m"),
		"empty timestamp":                 raw("v1..m"),
		"empty id":                        raw("v1.1."),
		"too few fields":                  raw("v1.1"),
		"non-numeric timestamp":           raw("v1.abc.m"),
		"plus sign":                       raw("v1.+1.m"),
		"leading zero":                    raw("v1.01.m"),
		"overflow":                        raw("v1.99999999999999999999.m"),
		"float":                           raw("v1.1,5.m"),
	} {
		if got, err := Decode(token); !errors.Is(err, ErrInvalid) || got != nil {
			t.Errorf("%s: Decode(%q) = %v, %v; want ErrInvalid", name, token, got, err)
		}
	}
}

func TestInvalidIsWrongParams(t *testing.T) {
	if !errors.Is(ErrInvalid, e.ErrorWrongParams) {
		t.Fatal("ErrInvalid does not wrap e.ErrorWrongParams: e.Handle would not answer 400")
	}
}

func TestIDMayContainDots(t *testing.T) {
	p := Position{CreatedAt: time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC), ID: "msg.1.a"}
	token, err := Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != p.ID || !got.CreatedAt.Equal(p.CreatedAt) {
		t.Fatalf("Decode = %+v, want %+v", got, p)
	}
}

func TestEncodeRejectsEmptyID(t *testing.T) {
	if _, err := Encode(Position{CreatedAt: time.Now()}); err == nil {
		t.Error("Encode(id=\"\") succeeded")
	}
}
