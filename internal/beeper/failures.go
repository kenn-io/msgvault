package beeper

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"go.kenn.io/msgvault/internal/store"
)

// phaseFetch items name a chat; every other phase names a message.
const phaseFetch = "fetch"

// ChatFailure is one chat's share of a run's recorded errors.
type ChatFailure struct {
	// ChatID is the Beeper chat ID, or empty when an item's message is not in
	// the archive and so cannot be tied to a chat.
	ChatID string
	// Title is the archived chat title; empty for untitled chats (usually
	// DMs) and for chats whose history has not been archived yet.
	Title  string
	Errors int
	// Phases lists the distinct failing phases (fetch, attachment, ...).
	Phases []string
	// LastError is the newest recorded error message for the chat.
	LastError string
}

// RunFailures groups a sync run's recorded error items by chat.
type RunFailures struct {
	// Total counts every error item recorded on the run. It can exceed the
	// items grouped into Chats when the listing limit was reached.
	Total  int64
	Listed int
	// Chats is ordered by error count, most first.
	Chats []ChatFailure
}

// RunFailures reads the error items a Beeper sync or media backfill run
// recorded and groups the newest limit of them by chat, resolving chat titles
// from the archive. It makes no Beeper API calls, so it works after Beeper
// Desktop has gone away.
func (imp *Importer) RunFailures(ctx context.Context, sourceID, syncRunID int64, limit int) (*RunFailures, error) {
	total, err := imp.store.CountSyncRunItems(syncRunID, store.SyncRunItemStatusError)
	if err != nil {
		return nil, fmt.Errorf("count sync run errors: %w", err)
	}
	out := &RunFailures{Total: total}
	if total == 0 {
		return out, nil
	}
	items, err := imp.store.ListSyncRunItemsContext(ctx, syncRunID, store.SyncRunItemStatusError, limit)
	if err != nil {
		return nil, fmt.Errorf("list sync run errors: %w", err)
	}
	out.Listed = len(items)

	var chatIDs, messageIDs []string
	for _, it := range items {
		if it.Phase == phaseFetch {
			chatIDs = append(chatIDs, it.SourceMessageID)
		} else {
			messageIDs = append(messageIDs, it.SourceMessageID)
		}
	}
	chats, err := imp.store.ConversationsBySourceConversationIDContext(ctx, sourceID, chatIDs)
	if err != nil {
		return nil, err
	}
	messageChats, err := imp.store.ConversationsBySourceMessageIDContext(ctx, sourceID, messageIDs)
	if err != nil {
		return nil, err
	}

	byChat := map[string]*ChatFailure{}
	var order []string
	// Items arrive newest first, so the first message seen per chat is the
	// newest one.
	for _, it := range items {
		var ref store.ConversationRef
		if it.Phase == phaseFetch {
			ref = chats[it.SourceMessageID]
			ref.SourceConversationID = it.SourceMessageID
		} else {
			ref = messageChats[it.SourceMessageID]
		}
		cf := byChat[ref.SourceConversationID]
		if cf == nil {
			cf = &ChatFailure{ChatID: ref.SourceConversationID, Title: ref.Title, LastError: it.ErrorMessage}
			byChat[ref.SourceConversationID] = cf
			order = append(order, ref.SourceConversationID)
		}
		cf.Errors++
		if !slices.Contains(cf.Phases, it.Phase) {
			cf.Phases = append(cf.Phases, it.Phase)
		}
	}
	for _, id := range order {
		out.Chats = append(out.Chats, *byChat[id])
	}
	sort.SliceStable(out.Chats, func(i, j int) bool { return out.Chats[i].Errors > out.Chats[j].Errors })
	return out, nil
}
