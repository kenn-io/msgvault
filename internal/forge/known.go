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
// when a sync last fetched it, from reading its first page to finishing.
type threadFacts struct {
	Ref       ThreadRef `json:"ref"`
	URL       string    `json:"url,omitempty"`
	ReadAt    time.Time `json:"read_at"`
	CheckedAt time.Time `json:"checked_at"`
}

// recordThread stores a fetched thread's facts on its conversation. A
// thread found by search keeps the reason its last notification gave. The
// title and link are cleaned as posts are.
func recordThread(ctx context.Context, st *store.Store, sourceID int64, head ThreadHead, ref ThreadRef, readAt, checkedAt time.Time) error {
	head.Title, head.URL = clean(head.Title), clean(head.URL)
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
	encoded, err := json.Marshal(threadFacts{Ref: ref, URL: head.URL, ReadAt: readAt.UTC(), CheckedAt: checkedAt.UTC()}, json.Deterministic(true))
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
	// archived is each archived thread's update when it was fetched, and
	// when that fetch read its first page, by key.
	archived map[string]archivedAt
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
	k.archived = make(map[string]archivedAt, len(ids))
	for _, id := range ids {
		facts, ok := decodeFacts(metadata[id])
		if ok {
			readAt := facts.ReadAt
			if readAt.IsZero() || readAt.After(facts.CheckedAt) {
				readAt = facts.CheckedAt
			}
			k.loaded = append(k.loaded, KnownThread{Ref: facts.Ref, ReadAt: readAt, CheckedAt: facts.CheckedAt})
			k.archived[facts.Ref.Key] = archivedAt{updated: facts.Ref.UpdatedAt, read: readAt}
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

// archivedAt is the update an archived thread was fetched for, and when the
// fetch read its first page.
type archivedAt struct {
	updated, read time.Time
}

// readMargin is how long after an update a fetch must have read a thread to
// hold it for certain: forges report updates in whole seconds, by a clock
// that may differ from this machine's.
const readMargin = time.Minute

// holds reports whether the archived copy holds an update discovery
// reports: one older than the update it was fetched for, or that same
// update when the fetch read the thread well after it. A thread changed
// again in the same second as the update it was fetched for reports that
// update again, and is fetched again unless its copy was read well after.
func (a archivedAt) holds(update time.Time) bool {
	return !update.IsZero() && (update.Before(a.updated) || update.Equal(a.updated) && a.read.After(update.Add(readMargin)))
}

// all returns every archived thread's archivedAt, by key.
func (k *knownThreads) all(ctx context.Context) (map[string]archivedAt, error) {
	if err := k.load(ctx); err != nil {
		return nil, err
	}
	return k.archived, nil
}
