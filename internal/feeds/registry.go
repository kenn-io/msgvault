// Package feeds archives the items of RSS, Atom and JSON feeds. All feeds
// belong to one "feed" source; each feed is a conversation of that source,
// and each item a feed_item message written through the provider-neutral
// message import. The feeds msgvault follows are stored in their
// conversations' metadata, so following feeds needs no schema change.
package feeds

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/messageimport"
	"go.kenn.io/msgvault/internal/store"
)

const (
	SourceType        = "feed"
	SourceIdentifier  = "feeds"
	SourceDisplayName = "Feeds"
	MessageType       = "feed_item"

	// metadataKey names the conversation metadata object that holds a feed's
	// subscription state.
	metadataKey = "feed"
	maxURLChars = 4096
)

var (
	ErrInvalidURL  = errors.New("feed URL must be an absolute http or https URL")
	ErrNotFollowed = errors.New("feed is not followed")
)

// Feed is one feed msgvault knows. A removed feed keeps its archived items
// but is no longer fetched.
type Feed struct {
	URL   string
	State State
}

// Followed reports whether syncs fetch the feed.
func (f Feed) Followed() bool {
	return f.State.RemovedAt == nil
}

// State is a feed's subscription record, stored under the "feed" key of
// its conversation's metadata.
type State struct {
	// URL is the feed's address. The conversation's own ID is the URL too,
	// unless the URL is too long for an ID; see conversationKey.
	URL          string     `json:"url,omitempty"`
	Title        string     `json:"title,omitempty"`
	AddedAt      *time.Time `json:"added_at,omitempty"`
	RemovedAt    *time.Time `json:"removed_at,omitempty"`
	ETag         string     `json:"etag,omitempty"`
	LastModified string     `json:"last_modified,omitempty"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
}

// Registry reads and changes the feeds msgvault follows.
type Registry struct {
	store *store.Store
	now   func() time.Time
}

func NewRegistry(st *store.Store) *Registry {
	return &Registry{store: st, now: time.Now}
}

// NormalizeURL validates a feed URL and returns it trimmed, with its scheme
// and host lowercased so one feed has one key.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len([]rune(raw)) > maxURLChars {
		return "", ErrInvalidURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", ErrInvalidURL
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String(), nil
}

// Source returns the feed source, creating it on first use.
func (r *Registry) Source(ctx context.Context) (*store.Source, error) {
	source, err := r.store.GetOrCreateSourceContext(ctx, SourceType, SourceIdentifier)
	if err != nil {
		return nil, fmt.Errorf("resolve feed source: %w", err)
	}
	if !source.DisplayName.Valid || strings.TrimSpace(source.DisplayName.String) == "" {
		if err := r.store.UpdateSourceDisplayNameContext(ctx, source.ID, SourceDisplayName); err != nil &&
			!errors.Is(err, store.ErrSourceSettingsInvalid) {
			return nil, fmt.Errorf("name feed source: %w", err)
		}
		source.DisplayName = sql.NullString{String: SourceDisplayName, Valid: true}
	}
	return source, nil
}

// Add follows a feed, or follows a removed feed again. It reports whether
// the feed was not followed before.
func (r *Registry) Add(ctx context.Context, rawURL, title string) (Feed, bool, error) {
	feedURL, err := NormalizeURL(rawURL)
	if err != nil {
		return Feed{}, false, err
	}
	source, err := r.Source(ctx)
	if err != nil {
		return Feed{}, false, err
	}
	var added bool
	state, err := r.update(ctx, source, feedURL, title, func(state *State) {
		if state.AddedAt == nil {
			now := r.now().UTC()
			state.AddedAt = &now
			added = true
		}
		if state.RemovedAt != nil {
			state.RemovedAt = nil
			added = true
		}
		if title != "" {
			state.Title = title
		}
	})
	if err != nil {
		return Feed{}, false, err
	}
	return Feed{URL: feedURL, State: state}, added, nil
}

// Remove stops fetching a feed. Its archived items stay.
func (r *Registry) Remove(ctx context.Context, rawURL string) (Feed, error) {
	feedURL, err := NormalizeURL(rawURL)
	if err != nil {
		return Feed{}, err
	}
	feeds, err := r.List(ctx)
	if err != nil {
		return Feed{}, err
	}
	index := slices.IndexFunc(feeds, func(f Feed) bool { return f.URL == feedURL })
	if index < 0 || !feeds[index].Followed() {
		return Feed{}, fmt.Errorf("%w: %s", ErrNotFollowed, feedURL)
	}
	source, err := r.Source(ctx)
	if err != nil {
		return Feed{}, err
	}
	state, err := r.update(ctx, source, feedURL, "", func(state *State) {
		now := r.now().UTC()
		state.RemovedAt = &now
	})
	if err != nil {
		return Feed{}, err
	}
	return Feed{URL: feedURL, State: state}, nil
}

// List returns every feed msgvault has followed, followed or removed, in the
// order they were first added.
func (r *Registry) List(ctx context.Context) ([]Feed, error) {
	source, err := r.store.GetSourceByTypeAndIdentifier(ctx, SourceType, SourceIdentifier)
	if errors.Is(err, store.ErrSourceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve feed source: %w", err)
	}
	ids, err := r.store.ListSourceConversationIDsContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	metadata, err := r.store.ConversationMetadataBatchContext(ctx, source.ID, ids)
	if err != nil {
		return nil, err
	}
	feeds := make([]Feed, 0, len(ids))
	for _, id := range ids {
		state, ok, err := decodeState(metadata[id])
		if err != nil {
			return nil, fmt.Errorf("read feed %s: %w", id, err)
		}
		if ok {
			feedURL := state.URL
			if feedURL == "" {
				feedURL = id
			}
			feeds = append(feeds, Feed{URL: feedURL, State: state})
		}
	}
	return feeds, nil
}

// update applies change to a feed's state through st, creating the feed's
// conversation if needed, and returns the stored state. A non-empty title
// also names the conversation.
func (r *Registry) update(ctx context.Context, source *store.Source, feedURL, title string, change func(*State)) (State, error) {
	return updateState(ctx, r.store, source, feedURL, title, change)
}

func updateState(
	ctx context.Context, st *store.Store, source *store.Source, feedURL, title string, change func(*State),
) (State, error) {
	conversationID, err := st.EnsureConversationWithTypeContext(
		ctx, source.ID, conversationKey(feedURL), messageimport.ConversationType, title)
	if err != nil {
		return State{}, fmt.Errorf("ensure feed conversation: %w", err)
	}
	stored, err := st.GetConversationMetadataContext(ctx, conversationID)
	if err != nil {
		return State{}, err
	}
	object := map[string]jsontext.Value{}
	if stored.Valid && strings.TrimSpace(stored.String) != "" {
		if err := json.Unmarshal([]byte(stored.String), &object); err != nil {
			return State{}, fmt.Errorf("decode feed conversation metadata: %w", err)
		}
	}
	var state State
	if raw, ok := object[metadataKey]; ok {
		if err := json.Unmarshal(raw, &state); err != nil {
			return State{}, fmt.Errorf("decode feed state: %w", err)
		}
	}
	change(&state)
	state.URL = feedURL
	encoded, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return State{}, fmt.Errorf("encode feed state: %w", err)
	}
	object[metadataKey] = encoded
	metadata, err := json.Marshal(object, json.Deterministic(true))
	if err != nil {
		return State{}, fmt.Errorf("encode feed conversation metadata: %w", err)
	}
	if err := st.SetConversationMetadataContext(ctx, conversationID, sql.NullString{String: string(metadata), Valid: true}); err != nil {
		return State{}, err
	}
	return state, nil
}

// conversationKey is a feed's conversation ID: its URL, or a hash of a URL
// too long for an import's conversation ID.
func conversationKey(feedURL string) string {
	if utf8.RuneCountInString(feedURL) <= maxExternalIDChars {
		return feedURL
	}
	return "sha256:" + hashParts(feedURL)
}

func decodeState(metadata sql.NullString) (State, bool, error) {
	if !metadata.Valid || strings.TrimSpace(metadata.String) == "" {
		return State{}, false, nil
	}
	var object map[string]jsontext.Value
	if err := json.Unmarshal([]byte(metadata.String), &object); err != nil {
		return State{}, false, err
	}
	raw, ok := object[metadataKey]
	if !ok {
		return State{}, false, nil
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, false, err
	}
	return state, true, nil
}
