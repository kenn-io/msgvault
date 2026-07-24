package api

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"go.kenn.io/msgvault/internal/mime"
	"golang.org/x/sync/singleflight"
)

// Inline fan-out limits and cache.
//
// Opening a message with embedded images makes the web shell request every
// distinct cid: reference (GET .../inline?cid=...). Without a cache each request
// reloaded the raw MIME and fully re-parsed it — enmime decodes AND SHA-256
// hashes every attachment — just to return one small image. A 25 MB message
// carrying up to 32 distinct cids meant ~32 full parses (~800 MB of repeated
// work), degrading the daemon and UI.
//
// inlineParseCache parses each message once (guarded by singleflight so a burst
// of concurrent first-fetches collapses to a single parse) and serves every cid
// from that result, retaining only the inline parts rather than the whole
// decoded attachment set. The constants below also cap what the daemon is
// willing to serve independent of the client caps, because the endpoint is
// directly reachable.
const (
	// maxInlineRawBytes rejects raw MIME larger than this before parsing. Gmail
	// caps a message (headers + body + attachments) at 25 MiB; base64 transfer
	// encoding inflates the raw wire form by ~4/3, so 40 MiB covers legitimate
	// messages while refusing to parse pathological blobs 32 times over.
	maxInlineRawBytes = 40 << 20 // 40 MiB
	// maxInlinePartBytes caps a single served inline part. Mirrors the client's
	// MAX_ARCHIVED_INLINE_IMAGE_BYTES (5 MiB) so the bound holds even when the
	// endpoint is called directly.
	maxInlinePartBytes = 5 << 20 // 5 MiB
	// maxInlinePartsPerMessage caps distinct inline parts the daemon will serve
	// for one message. Matches the client's MAX_ARCHIVED_INLINE_IMAGE_CIDS (32);
	// a message exceeding it is denied wholesale as a resource-exhaustion guard.
	maxInlinePartsPerMessage = 32
	// inlineCacheMaxEntries / inlineCacheMaxBytes bound the transient fan-out
	// cache. It only needs to hold the inline parts of the handful of messages a
	// user is actively viewing, so keep it small; entries are evicted LRU once
	// either bound is exceeded. Only inline parts (not full attachment sets) are
	// retained, so the byte charge stays well below the raw message sizes.
	inlineCacheMaxEntries = 16
	inlineCacheMaxBytes   = 64 << 20 // 64 MiB
)

// Sentinel outcomes recorded on a cache entry so repeated cids for a bad message
// stay cheap (no reload/reparse) and map to stable HTTP statuses.
var (
	errInlineRawTooLarge  = errors.New("inline: raw message exceeds size cap")
	errInlineTooManyParts = errors.New("inline: too many inline parts")
	errInlineRawNotFound  = errors.New("inline: raw message not found")
)

// inlinePart is one decoded inline MIME part ready to serve. contentType is the
// raw value from the parser; the handler lower-cases and validates it exactly as
// the pre-cache path did, preserving the served Content-Type byte-for-byte.
type inlinePart struct {
	contentType string
	content     []byte
}

// inlineCacheEntry holds the inline parts of one parsed message plus the byte
// charge it contributes to the cache. err records a terminal outcome (oversized
// raw, parse failure, too many parts) that applies to every cid of the message.
type inlineCacheEntry struct {
	parts map[string]inlinePart
	bytes int
	err   error
}

// inlineCacheItem is the LRU list payload: it ties an entry to its message id so
// eviction can delete the map key when a node is dropped from the back.
type inlineCacheItem struct {
	id    int64
	entry *inlineCacheEntry
}

// inlineParseCache is a bounded, concurrency-safe cache of parsed inline parts.
//
// Keying: message id alone. The archive is an append-only system of record, so a
// given id's raw MIME is immutable; distinct ids never share an entry, so there
// is no cross-message leak. (A content-identity key would additionally guard an
// id being rewritten in place, but verifying it would force a raw reload on every
// hit — the exact repeated I/O this cache exists to remove — so id is the sound
// identity here.)
type inlineParseCache struct {
	mu         sync.Mutex
	entries    map[int64]*list.Element
	order      *list.List
	totalBytes int
	maxEntries int
	maxBytes   int
	sf         singleflight.Group
}

func newInlineParseCache(maxEntries, maxBytes int) *inlineParseCache {
	return &inlineParseCache{
		entries:    make(map[int64]*list.Element),
		order:      list.New(),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
	}
}

// cacheItem recovers the typed payload of an LRU list element; the list only
// ever holds *inlineCacheItem values.
func cacheItem(el *list.Element) *inlineCacheItem {
	item, _ := el.Value.(*inlineCacheItem)
	return item
}

// get returns the cached entry for id, promoting it to most-recently-used.
func (c *inlineParseCache) get(id int64) (*inlineCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[id]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return cacheItem(el).entry, true
}

// put stores entry for id and evicts LRU nodes until both bounds hold.
func (c *inlineParseCache) put(id int64, entry *inlineCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[id]; ok {
		item := cacheItem(el)
		c.totalBytes += entry.bytes - item.entry.bytes
		item.entry = entry
		c.order.MoveToFront(el)
	} else {
		el := c.order.PushFront(&inlineCacheItem{id: id, entry: entry})
		c.entries[id] = el
		c.totalBytes += entry.bytes
	}
	// Evict from the back until under both caps. The byte cap keeps one entry
	// alive even when it alone exceeds maxBytes: serving a single large message
	// beats thrashing an empty cache.
	for c.order.Len() > c.maxEntries || (c.totalBytes > c.maxBytes && c.order.Len() > 1) {
		back := c.order.Back()
		if back == nil {
			break
		}
		item := cacheItem(back)
		c.order.Remove(back)
		delete(c.entries, item.id)
		c.totalBytes -= item.entry.bytes
	}
}

// parsed returns the parsed inline parts for id, loading and parsing the raw MIME
// at most once across concurrent callers. loadRaw fetches the raw bytes only on a
// miss.
func (c *inlineParseCache) parsed(
	ctx context.Context,
	id int64,
	loadRaw func(context.Context) ([]byte, error),
) (*inlineCacheEntry, error) {
	if e, ok := c.get(id); ok {
		return e, nil
	}
	v, err, _ := c.sf.Do(strconv.FormatInt(id, 10), func() (any, error) {
		if e, ok := c.get(id); ok {
			return e, nil
		}
		raw, err := loadRaw(ctx)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, errInlineRawNotFound
		}
		entry := buildInlineCacheEntry(raw)
		c.put(id, entry)
		return entry, nil
	})
	if err != nil {
		return nil, fmt.Errorf("resolve inline parts for message %d: %w", id, err)
	}
	entry, _ := v.(*inlineCacheEntry)
	return entry, nil
}

// buildInlineCacheEntry parses raw once and retains only its inline parts. Raw
// that is oversized, unparsable, or carries too many inline parts yields an entry
// whose err field is set, which is still cached so repeated cids stay cheap.
func buildInlineCacheEntry(raw []byte) *inlineCacheEntry {
	if len(raw) > maxInlineRawBytes {
		return &inlineCacheEntry{err: errInlineRawTooLarge}
	}
	parsed, err := mime.Parse(raw)
	if err != nil {
		return &inlineCacheEntry{err: fmt.Errorf("parse inline MIME: %w", err)}
	}
	parts := make(map[string]inlinePart)
	total := 0
	for _, att := range parsed.Attachments {
		if !att.IsInline {
			continue
		}
		if _, exists := parts[att.ContentID]; exists {
			continue
		}
		parts[att.ContentID] = inlinePart{contentType: att.ContentType, content: att.Content}
		total += len(att.Content)
	}
	if len(parts) > maxInlinePartsPerMessage {
		return &inlineCacheEntry{err: errInlineTooManyParts}
	}
	return &inlineCacheEntry{parts: parts, bytes: total}
}
