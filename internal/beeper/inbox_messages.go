package beeper

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/url"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

const (
	inboxMaxPages    = 4
	inboxMaxMessages = 128
)

type inboxCandidate struct{ ID, SortKey string }

// LocalComplete means only that this bounded cursor walk reached an explicitly
// terminal page. It says nothing about the initial edge, cross-page snapshots,
// background indexing, network catch-up, or a sufficient current incoming window.
type inboxWindow struct {
	LocalComplete bool
	Candidates    []inboxCandidate
	Evidence      []inboxObject
	Stopped       string
}

func inboxIncomingCandidate(raw []byte, target inboxcontrol.Target) (inboxCandidate, bool, error) {
	var message Message
	if err := json.Unmarshal(raw, &message); err != nil {
		return inboxCandidate{}, false, inboxcontrol.ErrUnavailable
	}
	if message.ID == "" || message.ChatID != target.ProviderID || message.AccountID != target.AccountID || message.SortKey == "" || message.Timestamp.IsZero() {
		return inboxCandidate{}, false, inboxcontrol.ErrUnavailable
	}
	object, err := decodeInboxObject(message.Raw)
	if err != nil {
		return inboxCandidate{}, false, err
	}
	sender := inboxBoolean(object["isSender"])
	if sender == nil || *sender {
		return inboxCandidate{}, false, nil
	}
	return inboxCandidate{ID: message.ID, SortKey: message.SortKey}, true, nil
}

func (p *InboxProvider) localInboxWindow(ctx context.Context, target inboxcontrol.Target) inboxWindow {
	window := inboxWindow{Stopped: "page-limit"}
	if !p.validTarget(&target) {
		window.Stopped = "identity"
		return window
	}
	ctx, cancel := context.WithTimeout(ctx, inboxObservationTimeout)
	defer cancel()
	cursor := ""
	seenCursors := map[string]bool{"": true}
	seenIDs := map[string]bool{}
	for range inboxMaxPages {
		path := "/v1/chats/" + url.PathEscape(target.ProviderID) + "/messages"
		if cursor != "" {
			q := url.Values{"cursor": {cursor}, "direction": {"before"}}
			path += "?" + q.Encode()
		}
		page, err := p.getObject(ctx, path)
		if err != nil {
			window.Stopped = "response"
			return window
		}
		more := inboxBoolean(page["hasMore"])
		old, oldOK := inboxString(page["oldestCursor"])
		_, newOK := inboxString(page["newestCursor"])
		var items []jsontext.Value
		if more == nil || !oldOK || !newOK || json.Unmarshal(page["items"], &items) != nil || items == nil {
			window.Stopped = "envelope"
			return window
		}
		if len(items) > inboxMaxMessages-len(seenIDs) {
			window.Stopped = "message-limit"
			return window
		}
		evidence := inboxObject{"hasMore": page["hasMore"], "oldestCursor": page["oldestCursor"], "newestCursor": page["newestCursor"]}
		var messages []inboxObject
		for _, raw := range items {
			object, err := decodeInboxObject(raw)
			if err != nil {
				window.Stopped = "message"
				return window
			}
			id, idOK := inboxString(object["id"])
			if !idOK || id == "" || seenIDs[id] {
				window.Stopped = "duplicate-or-invalid-id"
				return window
			}
			seenIDs[id] = true
			candidate, incoming, err := inboxIncomingCandidate(raw, target)
			if err != nil {
				window.Stopped = "message"
				return window
			}
			if incoming {
				window.Candidates = append(window.Candidates, candidate)
			}
			metadata := inboxObject{}
			for _, key := range []string{"id", "chatID", "accountID", "sortKey", "timestamp", "isSender", "isHidden", "isDeleted"} {
				if value, ok := object[key]; ok {
					metadata[key] = value
				}
			}
			messages = append(messages, metadata)
		}
		encoded, err := json.Marshal(messages, json.Deterministic(true))
		if err != nil {
			window.Stopped = "message"
			return window
		}
		evidence["items"] = encoded
		window.Evidence = append(window.Evidence, evidence)
		if !*more {
			window.LocalComplete = true
			window.Stopped = "terminal-local-page"
			return window
		}
		if len(items) == 0 || old == "" || seenCursors[old] {
			window.Stopped = "cursor-progress"
			return window
		}
		seenCursors[old] = true
		cursor = old
	}
	return window
}
