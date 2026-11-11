package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

type changesFilter struct {
	PersonID                   int64
	SourceIDs, ConversationIDs []int64
	IncomingOnly               bool
	participants               []int64
}

func parseChangesFilter(r *http.Request) (changesFilter, error) {
	var f changesFilter
	var err error
	f.PersonID, _, err = queryInt64(r, "person_id")
	if err != nil {
		return f, err
	}
	if r.URL.Query().Has("person_id") && f.PersonID <= 0 {
		return f, newParamError("person_id", "person_id must be positive")
	}
	for _, item := range []struct {
		name   string
		target *[]int64
	}{{"source_id", &f.SourceIDs}, {"conversation_id", &f.ConversationIDs}} {
		values, _, e := queryInt64s(r, item.name)
		if e != nil {
			return f, e
		}
		if len(values) > 50 {
			return f, newParamError(item.name, item.name+" accepts at most 50 values")
		}
		for _, id := range values {
			if id <= 0 {
				return f, newParamError(item.name, item.name+" values must be positive")
			}
		}
		slices.Sort(values)
		*item.target = slices.Compact(values)
	}
	f.IncomingOnly, _, err = queryBool(r, "incoming_only")
	if err != nil {
		return f, err
	}
	if r.URL.Query().Has("incoming_only") && r.URL.Query().Get("incoming_only") == "" {
		return f, newParamError("incoming_only", "incoming_only must be a boolean")
	}
	return f, nil
}

func (f changesFilter) digest() string {
	if f.PersonID == 0 && len(f.SourceIDs) == 0 && len(f.ConversationIDs) == 0 && !f.IncomingOnly {
		return ""
	}
	data, err := json.Marshal(struct {
		PersonID                   int64
		SourceIDs, ConversationIDs []int64
		IncomingOnly               bool
	}{f.PersonID, f.SourceIDs, f.ConversationIDs, f.IncomingOnly}, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func (f changesFilter) matches(m store.ChangedMessage) bool {
	return (f.PersonID == 0 || slices.Contains(f.participants, m.SenderID)) &&
		(len(f.SourceIDs) == 0 || slices.Contains(f.SourceIDs, m.SourceID)) &&
		(len(f.ConversationIDs) == 0 || slices.Contains(f.ConversationIDs, m.ConversationID)) &&
		(!f.IncomingOnly || !m.IsFromMe)
}
