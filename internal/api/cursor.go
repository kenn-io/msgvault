package api

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/store"
)

// changesPosition is a decoded change-feed cursor: a keyset position, plus the
// archive it is a position IN.
//
// present is false for the start of the archive — an absent or empty cursor
// parameter — which names no archive because it names no position. The zero
// store.ChangedMessagesCursor is that start, so the zero changesPosition is
// usable as it stands.
type changesPosition struct {
	cursor  store.ChangedMessagesCursor
	archive string
	filter  string
	present bool
}

// decodeChangesCursor reads the SHAPE of a token this server issued: its format
// version, its envelope, and the position inside it. Every failure is a
// paramError, so the handler reports it as invalid_cursor rather than falling
// back to the start of the archive and re-delivering everything.
//
// Which archive the position belongs to is checked separately, by boundTo. The
// split is what keeps a client typo a 400 on every backend: reading the archive
// identity is a store capability, and a store that lacks it answers the route
// with 503 — which must not be allowed to mask a malformed cursor.
//
// What neither step does is AUTHENTICATE the token: any well-formed
// current-format cursor naming this archive is accepted, whoever built it.
func decodeChangesCursor(raw string) (changesPosition, error) {
	prefix, encoded, hasPrefix := strings.Cut(raw, ".")
	version, numeric := parseChangesCursorVersion(prefix)
	if !hasPrefix || !numeric {
		return changesPosition{}, changesCursorError("is not a cursor this API issued")
	}
	if version != changefeed.UnfilteredCursorVersion && version != changefeed.FilteredCursorVersion {
		return changesPosition{}, changesCursorError(fmt.Sprintf(
			"was issued by an incompatible version of this server (format %d, this "+
				"server speaks through %d); the sync cannot resume from it and must start "+
				"again from the beginning of the archive",
			version, changefeed.FilteredCursorVersion))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return changesPosition{}, changesCursorError("is not a cursor this API issued")
	}
	var payload changefeed.CursorPayload
	if version == changefeed.UnfilteredCursorVersion {
		// Decode the original shape so unknown fields retain their v1 meaning.
		var legacy struct {
			Time    string `json:"t"`
			ID      *int64 `json:"i,omitzero"`
			Archive string `json:"a"`
		}
		if err := json.Unmarshal(decoded, &legacy); err != nil {
			return changesPosition{}, changesCursorError("is not a cursor this API issued")
		}
		payload.Time, payload.ID, payload.Archive = legacy.Time, legacy.ID, legacy.Archive
	} else if err := json.Unmarshal(decoded, &payload); err != nil || payload.Filter == "" {
		return changesPosition{}, changesCursorError("is not a cursor this API issued")
	}
	at, err := time.Parse(changesTimeLayout, payload.Time)
	if err != nil {
		return changesPosition{}, changesCursorError("is not a cursor this API issued")
	}
	cursor := store.ChangedMessagesFrom(at.UTC())
	if payload.ID != nil {
		cursor = store.ChangedMessagesAfter(at.UTC(), *payload.ID)
	}
	return changesPosition{cursor: cursor, archive: payload.Archive, filter: payload.Filter, present: true}, nil
}

// boundTo rejects a position that was issued against a different archive, and is
// the whole point of carrying the UID. Rejecting is the only safe answer: the id
// half of the position is local to the archive that issued it, so honouring the
// cursor would resume the walk at an unrelated row and silently never deliver
// the records before it.
//
// The start of the archive passes unconditionally — it names no position, so
// there is nothing to misplace.
func (p changesPosition) boundTo(archiveUID string) error {
	if !p.present || p.archive == archiveUID {
		return nil
	}
	return changesCursorError(
		"was issued against a different archive; the position it carries is " +
			"local to that archive and means nothing here, so the sync cannot " +
			"resume from it and must start again from the beginning of this archive")
}

// parseChangesCursorVersion reads the token's leading format number, reporting
// whether it is one. strconv alone is too generous: it accepts a sign, and `+`
// would have to be escaped in a query string, so a token carrying one cannot be
// one this server issued.
func parseChangesCursorVersion(prefix string) (int, bool) {
	if prefix == "" {
		return 0, false
	}
	for _, r := range prefix {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	version, err := strconv.Atoi(prefix)
	return version, err == nil
}

func changesCursorError(detail string) error {
	return newParamError("cursor", `query parameter "cursor" `+detail)
}

// queryChangesCursor reads the feed position from a request. An absent or empty
// value is the start of the archive, matching how the rest of this API reads an
// empty query value.
func queryChangesCursor(r *http.Request) (changesPosition, error) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return changesPosition{}, nil
	}
	return decodeChangesCursor(raw)
}
