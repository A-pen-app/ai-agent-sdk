package models

import (
	"bytes"
	"encoding/json"
)

// Optional distinguishes an absent field from an explicit null in PATCH bodies:
//   - field absent          → Present=false
//   - field present as null → Present=true, Value=nil
//   - field present w/value → Present=true, Value=&v
//
// encoding/json only invokes UnmarshalJSON for keys that appear in the body,
// so Present reliably reports field presence.
type Optional[T any] struct {
	Present bool
	Value   *T
}

func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Present = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}

func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.Present || o.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}
