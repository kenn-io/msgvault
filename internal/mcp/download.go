package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/query"
)

const (
	MaxDownloadBytes = 256 << 20
	maxDownloads     = 8
	downloadLifetime = 5 * time.Minute
)

var (
	errDownloadExpired  = errors.New("download expired or sha256 does not match; restart at offset 0 without sha256")
	errDownloadTooLarge = errors.New("chunk downloads are limited to 256 MiB per object")
)

type downloadKey struct {
	operation  string
	identity   string
	message    query.MessageRef
	attachment int64
}

type downloadSnapshot struct {
	etag       string
	mediaType  string
	validate   func(context.Context) error
	data       []byte
	digest     string
	original   *query.OriginalMessage
	attachment *attachmentPayload
	expires    time.Time
	timer      *time.Timer
}

type downloadRefresh struct {
	active  int
	next    uint64
	applied uint64
}

// downloadCache retains at most 256 MiB across eight downloads. A digest pins
// continuations to their initial bytes until a fresh read replaces the snapshot.
type downloadCache struct {
	mu      sync.Mutex
	entries map[downloadKey]*downloadSnapshot
	loading map[downloadKey]chan struct{}
	refresh map[downloadKey]*downloadRefresh
	bytes   int
	closed  bool
}

func (c *downloadCache) get(ctx context.Context, key downloadKey, req chunkRequest, load func() (*downloadSnapshot, error)) (*downloadSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
retry:
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.closed {
		return nil, errDownloadExpired
	}
	if entry := c.entries[key]; entry != nil && (key.operation == "" || req.offset != 0) {
		if time.Now().Before(entry.expires) {
			if entry.validate != nil {
				if err := entry.validate(ctx); err != nil {
					if !errors.Is(err, errDownloadExpired) {
						return nil, err
					}
					c.remove(key)
					entry = nil
				}
			}
			if entry != nil {
				if req.digest != "" && req.digest != entry.digest {
					return nil, errDownloadExpired
				}
				return entry, nil
			}
		} else {
			c.remove(key)
		}
	}
	if req.offset != 0 {
		return nil, errDownloadExpired
	}
	if key.operation == "" {
		if pending := c.loading[key]; pending != nil {
			err := func() error {
				c.mu.Unlock()
				defer c.mu.Lock()
				select {
				case <-pending:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}()
			if err != nil {
				return nil, err
			}
			goto retry
		}
		if c.loading == nil {
			c.loading = make(map[downloadKey]chan struct{})
		}
		pending := make(chan struct{})
		c.loading[key] = pending
		defer func() {
			delete(c.loading, key)
			close(pending)
		}()
	}
	var refresh *downloadRefresh
	var sequence uint64
	if key.operation != "" {
		if c.refresh == nil {
			c.refresh = make(map[downloadKey]*downloadRefresh)
		}
		refresh = c.refresh[key]
		if refresh == nil {
			refresh = &downloadRefresh{}
			c.refresh[key] = refresh
		}
		refresh.active++
		refresh.next++
		sequence = refresh.next
		defer func() {
			refresh.active--
			if refresh.active == 0 {
				delete(c.refresh, key)
			}
		}()
	}
	entry, err := func() (*downloadSnapshot, error) {
		c.mu.Unlock()
		defer c.mu.Lock()
		entry, err := load()
		if err != nil {
			return nil, err
		}
		if len(entry.data) > MaxDownloadBytes {
			return nil, errDownloadTooLarge
		}
		sum := sha256.Sum256(entry.data)
		entry.digest = hex.EncodeToString(sum[:])
		return entry, nil
	}()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.closed {
		return nil, errDownloadExpired
	}
	if key.operation == "" && c.entries[key] != nil {
		goto retry
	}
	if req.digest != "" && req.digest != entry.digest {
		return nil, errDownloadExpired
	}
	if key.operation != "" {
		// A later successful refresh owns the cache if this read finished last.
		if sequence < refresh.applied {
			return entry, nil
		}
		refresh.applied = sequence
		if c.entries[key] != nil {
			c.remove(key)
		}
		if req.offset == 0 && int64(len(entry.data)) <= req.length {
			return entry, nil
		}
	}
	for len(c.entries) >= maxDownloads || c.bytes+len(entry.data) > MaxDownloadBytes {
		var oldest downloadKey
		var expires time.Time
		for key, cached := range c.entries {
			if expires.IsZero() || cached.expires.Before(expires) {
				oldest, expires = key, cached.expires
			}
		}
		c.remove(oldest)
	}
	if c.entries == nil {
		c.entries = make(map[downloadKey]*downloadSnapshot)
	}
	entry.expires = time.Now().Add(downloadLifetime)
	c.entries[key] = entry
	c.bytes += len(entry.data)
	entry.timer = time.AfterFunc(downloadLifetime, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.entries[key] == entry {
			c.remove(key)
		}
	})
	return entry, nil
}

func (c *downloadCache) remove(key downloadKey) {
	entry := c.entries[key]
	entry.timer.Stop()
	c.bytes -= len(entry.data)
	delete(c.entries, key)
}

func (c *downloadCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for key := range c.entries {
		c.remove(key)
	}
}
