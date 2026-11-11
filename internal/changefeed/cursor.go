package changefeed

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type cursorPayload struct {
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
	payload, err := json.Marshal(cursorPayload{
		Time:    position.At().UTC().Format(time.RFC3339Nano),
		ID:      id,
		Archive: archiveUID,
		Filter:  digest,
	}, json.Deterministic(true))
	if err != nil {
		// Unreachable: every field of the payload marshals unconditionally.
		panic(fmt.Sprintf("encode changes cursor: %v", err))
	}
	version := 1
	if digest != "" {
		version = 2
	}
	return strconv.Itoa(version) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
}
