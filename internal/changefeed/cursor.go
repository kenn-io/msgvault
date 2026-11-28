package changefeed

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Cursor versions prefix the envelope so incompatible formats can be recognized.
const (
	UnfilteredCursorVersion = 1
	FilteredCursorVersion   = 2
)

// CursorPayload binds an unsigned position to its archive and optional filter.
// ID is omitted at the start of an instant; every integer is a valid row ID.
type CursorPayload struct {
	Time    string `json:"t"`
	ID      *int64 `json:"i,omitzero" nullable:"false"`
	Archive string `json:"a"`
	Filter  string `json:"f,omitzero"`
}

func EncodeCursor(archiveUID string, position store.ChangedMessagesCursor, digest string) string {
	var id *int64
	if after, ok := position.AfterID(); ok {
		id = &after
	}
	payload, err := json.Marshal(CursorPayload{
		Time:    position.At().UTC().Format(time.RFC3339Nano),
		ID:      id,
		Archive: archiveUID,
		Filter:  digest,
	}, json.Deterministic(true))
	if err != nil {
		// Unreachable: every field of the payload marshals unconditionally.
		panic(fmt.Sprintf("encode changes cursor: %v", err))
	}
	version := UnfilteredCursorVersion
	if digest != "" {
		version = FilteredCursorVersion
	}
	return strconv.Itoa(version) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
}
