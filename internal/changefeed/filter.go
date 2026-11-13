package changefeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"slices"

	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

type GrantRef struct{ Type, Identifier string }
type Filter struct {
	PersonID                   int64
	SourceIDs, ConversationIDs []int64
	IncomingOnly               bool
	Participants               []int64
	GrantRefs                  []GrantRef
}

func (f Filter) HasSelectors() bool {
	return f.PersonID != 0 || len(f.SourceIDs) != 0 || len(f.ConversationIDs) != 0 || f.IncomingOnly
}

func ResolveFilter(ctx context.Context, profiles any, f Filter) (Filter, error) {
	if f.PersonID != 0 {
		resolution, err := resolver.Resolve(ctx, profiles, resolver.Reference{Kind: resolver.ReferencePerson, ID: f.PersonID}, nil)
		if err != nil {
			return Filter{}, err
		}
		f.Participants = resolution.Scope.ParticipantIDs
	}
	return f, nil
}
func (f Filter) Digest() string {
	if !f.HasSelectors() && f.GrantRefs == nil {
		return ""
	}
	// Omit nil grant references to preserve the owner's filter encoding.
	data, err := json.Marshal(struct {
		PersonID                   int64
		SourceIDs, ConversationIDs []int64
		IncomingOnly               bool
		GrantRefs                  []GrantRef `json:",omitzero"`
	}{f.PersonID, f.SourceIDs, f.ConversationIDs, f.IncomingOnly, f.GrantRefs}, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
func (f Filter) Matches(m store.ChangedMessage) bool {
	return (f.PersonID == 0 || slices.Contains(f.Participants, m.SenderID)) && (len(f.SourceIDs) == 0 || slices.Contains(f.SourceIDs, m.SourceID)) && (len(f.ConversationIDs) == 0 || slices.Contains(f.ConversationIDs, m.ConversationID)) && (!f.IncomingOnly || !m.IsFromMe)
}

const ScanBudget = 2000

func Scan(page store.ChangedMessagePage, since store.ChangedMessagesCursor, f Filter, limit int) ([]store.ChangedMessage, store.ChangedMessagesCursor, bool) {
	rows := page.Messages
	matches := make([]store.ChangedMessage, 0, limit)
	next := since
	examined := 0
	for i, row := range rows {
		if i == ScanBudget {
			break
		}
		examined++
		next = store.ChangedMessagesAfter(row.ContentChangedAt, row.ID)
		if f.Matches(row) {
			matches = append(matches, row)
			if len(matches) == limit {
				return matches, next, i+1 < len(rows)
			}
		}
	}
	if len(rows) > examined {
		return matches, next, true
	}
	if !page.CompleteThrough.IsZero() && (page.CompleteThrough.After(since.At()) || since.At().After(page.ServerTime)) {
		next = store.ChangedMessagesFrom(page.CompleteThrough)
	}
	return matches, next, false
}
