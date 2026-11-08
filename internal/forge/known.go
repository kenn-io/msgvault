package forge

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// conversationMetadataKey holds a thread's facts in its conversation's
// metadata, beside whatever else the archive keeps there.
const conversationMetadataKey = "forge"

// threadFacts is what a thread's conversation records about it: enough to
// fetch it again, its URL, why the forge last notified the account, and
// when a sync last fetched it.
type threadFacts struct {
	Ref       ThreadRef `json:"ref"`
	URL       string    `json:"url,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// recordThread stores a fetched thread's facts on its conversation. A
// thread found by search keeps the reason its last notification gave.
func recordThread(ctx context.Context, st *store.Store, sourceID int64, head ThreadHead, ref ThreadRef, checkedAt time.Time) error {
	conversationID, err := st.EnsureConversationWithTypeContext(ctx, sourceID, head.ConversationID, ConversationType, head.Title)
	if err != nil {
		return fmt.Errorf("resolve conversation of %s: %w", ref.Name(), err)
	}
	stored, err := st.GetConversationMetadataContext(ctx, conversationID)
	if err != nil {
		return fmt.Errorf("read conversation metadata of %s: %w", ref.Name(), err)
	}
	object := map[string]jsontext.Value{}
	if stored.Valid && strings.TrimSpace(stored.String) != "" {
		if err := json.Unmarshal([]byte(stored.String), &object); err != nil {
			return fmt.Errorf("decode conversation metadata of %s: %w", ref.Name(), err)
		}
	}
	if raw, ok := object[conversationMetadataKey]; ok && ref.Reason == "" {
		var previous threadFacts
		if json.Unmarshal(raw, &previous) == nil {
			ref.Reason = previous.Ref.Reason
		}
	}
	encoded, err := json.Marshal(threadFacts{Ref: ref, URL: head.URL, CheckedAt: checkedAt.UTC()}, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode facts of %s: %w", ref.Name(), err)
	}
	object[conversationMetadataKey] = encoded
	metadata, err := json.Marshal(object, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode conversation metadata of %s: %w", ref.Name(), err)
	}
	return st.SetConversationMetadataContext(ctx, conversationID, sql.NullString{String: string(metadata), Valid: true})
}

// knownThreads reads a source's archived threads from their conversations'
// facts, once per sync.
type knownThreads struct {
	store    *store.Store
	sourceID int64
	loaded   []KnownThread
	ready    bool
}

func (k *knownThreads) load(ctx context.Context) error {
	if k.ready {
		return nil
	}
	ids, err := k.store.ListSourceConversationIDsContext(ctx, k.sourceID)
	if err != nil {
		return fmt.Errorf("list archived threads: %w", err)
	}
	metadata, err := k.store.ConversationMetadataBatchContext(ctx, k.sourceID, ids)
	if err != nil {
		return fmt.Errorf("read archived threads: %w", err)
	}
	for _, id := range ids {
		facts, ok := decodeFacts(metadata[id])
		if ok {
			k.loaded = append(k.loaded, KnownThread{Ref: facts.Ref, CheckedAt: facts.CheckedAt})
		}
	}
	slices.SortStableFunc(k.loaded, func(a, b KnownThread) int {
		return cmp.Or(a.CheckedAt.Compare(b.CheckedAt), strings.Compare(a.Ref.Key, b.Ref.Key))
	})
	k.ready = true
	return nil
}

func decodeFacts(metadata sql.NullString) (threadFacts, bool) {
	if !metadata.Valid || strings.TrimSpace(metadata.String) == "" {
		return threadFacts{}, false
	}
	var object map[string]jsontext.Value
	if json.Unmarshal([]byte(metadata.String), &object) != nil {
		return threadFacts{}, false
	}
	raw, ok := object[conversationMetadataKey]
	if !ok {
		return threadFacts{}, false
	}
	var facts threadFacts
	if json.Unmarshal(raw, &facts) != nil || facts.Ref.Key == "" {
		return threadFacts{}, false
	}
	return facts, true
}

func (k *knownThreads) Oldest(ctx context.Context, limit int) ([]KnownThread, error) {
	if err := k.load(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	return slices.Clone(k.loaded[:min(limit, len(k.loaded))]), nil
}

func (k *knownThreads) Each(ctx context.Context, fn func(KnownThread) error) error {
	if err := k.load(ctx); err != nil {
		return err
	}
	for _, thread := range k.loaded {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(thread); err != nil {
			return err
		}
	}
	return nil
}
