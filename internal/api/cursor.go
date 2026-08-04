package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// changesCursorVersion is the only cursor format this server speaks. It is
// carried as a `<n>.` prefix OUTSIDE the encoded part, and it is the single
// source of truth for the format — there is deliberately no second version
// marker inside the payload.
//
// Outside is what makes it useful. A version buried in the payload is
// unreadable until the reader has already agreed that the payload is
// base64url-wrapped JSON, so it can only version the payload's fields; any
// later format that changed the envelope — signed, binary — would reach its
// holder as indistinguishable from corruption. Out here it versions the
// envelope too, so a cursor from any future format is still recognised as one,
// and its holder is told to restart rather than left guessing.
const changesCursorVersion = 1

// changesCursorPayload is the encoded part of a change-feed cursor.
//
// The token is neither signed nor encrypted, deliberately. The API server has
// no secret to sign with, and a forged cursor only moves the caller's own
// position in the caller's own feed — it reaches no message the caller could
// not already request. Tamper-evidence would buy nothing.
//
// Archive is the durable UID of the archive the cursor was issued against, and
// it is a correctness field rather than a security one. The position it carries
// is a watermark plus an ARCHIVE-LOCAL message id: it means nothing anywhere
// else, and honouring one aimed at a different --db, a subset copy, or an
// archive rebuilt from scratch would start the walk at an unrelated position and
// silently omit everything before it. A file-level restore of the SAME archive
// keeps its UID, so a cursor survives one — which is the point of binding to the
// durable identity rather than to the file.
//
// Unsigned, it stops an accident, not an attacker — and the accident is the
// failure mode this feed exists to rule out.
//
// ID is a POINTER because the position's id half is optional, not because it
// can be null on the wire: a position at the start of an instant has no id
// tiebreak at all, and it must not be encoded as one. Omitted rather than sent
// as a number, because no number means it — every int64 is a legal message id
// (see store.ChangedMessagesCursor), so any sentinel value would come back as
// "after that row" and skip the rows at or below it.
type changesCursorPayload struct {
	Time    string `json:"t"`
	ID      *int64 `json:"i,omitempty"`
	Archive string `json:"a"`
}

// encodeChangesCursor renders a feed position in archiveUID as the opaque token
// clients send back. Both the digits and the `.` are unreserved in a query
// string and RawURLEncoding keeps the rest so too, so the whole token needs no
// escaping.
func encodeChangesCursor(archiveUID string, position store.ChangedMessagesCursor) string {
	var id *int64
	if after, ok := position.AfterID(); ok {
		id = &after
	}
	payload, err := json.Marshal(changesCursorPayload{
		Time:    position.At().UTC().Format(changesTimeLayout),
		ID:      id,
		Archive: archiveUID,
	})
	if err != nil {
		// Unreachable: every field of the payload marshals unconditionally.
		panic(fmt.Sprintf("encode changes cursor: %v", err))
	}
	return strconv.Itoa(changesCursorVersion) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
}

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
	if version != changesCursorVersion {
		return changesPosition{}, changesCursorError(fmt.Sprintf(
			"was issued by an incompatible version of this server (format %d, this "+
				"server speaks %d); the sync cannot resume from it and must start "+
				"again from the beginning of the archive",
			version, changesCursorVersion))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return changesPosition{}, changesCursorError("is not a cursor this API issued")
	}
	var payload changesCursorPayload
	if err := json.Unmarshal(decoded, &payload); err != nil {
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
	return changesPosition{cursor: cursor, archive: payload.Archive, present: true}, nil
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
