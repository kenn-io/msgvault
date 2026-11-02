package threads

import (
	"crypto/sha256"
	"strings"
)

// CursorTracker belongs to one pinned walk, never a shared Client. Keep it
// across pages to detect longer cursor cycles and repeated pages with new
// cursors. On resume seed the client with the checkpoint's current After.
type CursorTracker struct {
	cursors map[string]bool
	pages   map[[32]byte]bool
}

func (p *CursorTracker) Next(page Page) (string, error) {
	if len(page.Items) == 0 {
		return "", nil
	}
	if page.After == "" || p.cursors[page.After] {
		return "", ErrPagination
	}
	var ids strings.Builder
	for _, item := range page.Items {
		ids.WriteString(item.ID)
		ids.WriteByte(0)
	}
	fingerprint := sha256.Sum256([]byte(ids.String()))
	if p.pages[fingerprint] {
		return "", ErrPagination
	}
	if p.cursors == nil {
		p.cursors = make(map[string]bool)
		p.pages = make(map[[32]byte]bool)
	}
	p.cursors[page.After] = true
	p.pages[fingerprint] = true
	return page.After, nil
}
