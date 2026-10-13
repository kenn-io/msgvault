package microsoft

import (
	"os"
	"sync"
)

type graphGrantCacheEntry struct {
	info   os.FileInfo
	scopes []string
}

// graphGrantedScopesCache avoids reparsing an unchanged token file while
// checking the current native grant. Atomic token replacement changes the
// file identity; in-process writes also invalidate the entry directly.
type graphGrantedScopesCache struct {
	mu      sync.Mutex
	entries map[string]graphGrantCacheEntry
}

func (c *graphGrantedScopesCache) get(path string, load func() (*tokenFile, error)) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		delete(c.entries, path)
		return nil, err
	}
	if entry, ok := c.entries[path]; ok && sameGraphGrantFile(entry.info, info) {
		return append([]string(nil), entry.scopes...), nil
	}
	tf, err := load()
	if err != nil {
		delete(c.entries, path)
		return nil, err
	}
	if c.entries == nil {
		c.entries = make(map[string]graphGrantCacheEntry)
	}
	c.entries[path] = graphGrantCacheEntry{info: info, scopes: append([]string(nil), tf.Scopes...)}
	return append([]string(nil), tf.Scopes...), nil
}

func (c *graphGrantedScopesCache) invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, path)
}

func sameGraphGrantFile(cached, current os.FileInfo) bool {
	return cached != nil && os.SameFile(cached, current) && cached.Size() == current.Size() && cached.ModTime().Equal(current.ModTime())
}
