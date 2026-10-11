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
	maxDownloadBytes = 256 << 20
	maxDownloads     = 8
	downloadLifetime = 5 * time.Minute
)

var (
	errDownloadExpired  = errors.New("download expired or sha256 does not match; restart at offset 0 without sha256")
	errDownloadTooLarge = errors.New("chunk downloads are limited to 256 MiB per object")
)

type downloadKey struct {
	message    query.MessageRef
	attachment int64
}

type downloadSnapshot struct {
	validate   func(context.Context) error
	data       []byte
	digest     string
	original   *query.OriginalMessage
	attachment *attachmentPayload
	expires    time.Time
	timer      *time.Timer
}

// downloadCache retains at most 256 MiB across eight downloads. A digest pins
// continuations to their initial bytes even if the archive changes meanwhile.
type downloadCache struct {
	mu      sync.Mutex
	entries map[downloadKey]*downloadSnapshot
	bytes   int
	closed  bool
}

func (c *downloadCache) get(ctx context.Context, key downloadKey, req chunkRequest, load func() (*downloadSnapshot, error)) (*downloadSnapshot, error) {
	// ponytail: serialize snapshot loads; per-download locks if concurrent
	// large exports need to avoid waiting behind another download.
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.closed {
		return nil, errDownloadExpired
	}
	if entry := c.entries[key]; entry != nil {
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
	entry, err := load()
	if err != nil {
		return nil, err
	}
	if len(entry.data) > maxDownloadBytes {
		return nil, errDownloadTooLarge
	}
	sum := sha256.Sum256(entry.data)
	entry.digest = hex.EncodeToString(sum[:])
	if req.digest != "" && req.digest != entry.digest {
		return nil, errDownloadExpired
	}
	for len(c.entries) >= maxDownloads || c.bytes+len(entry.data) > maxDownloadBytes {
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
