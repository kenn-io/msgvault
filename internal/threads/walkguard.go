package threads

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// WalkGuard uses Brent's cycle detection across checkpoints. Its storage stays
// constant even for very large conversations. Repeated adjacent pages fail
// immediately; longer cursor/page cycles fail when they reach the saved anchor.
type WalkGuard struct {
	AnchorCursor  string `json:"anchor_cursor,omitempty"`
	AnchorPage    string `json:"anchor_page,omitempty"`
	LastPage      string `json:"last_page,omitempty"`
	Power         uint64 `json:"power,omitzero"`
	Distance      uint64 `json:"distance,omitzero"`
	Invalidations int    `json:"invalidations,omitzero"`
}

func (g *WalkGuard) observe(before string, p Page) error {
	if len(p.Items) == 0 {
		return nil
	}
	var ids strings.Builder
	for _, item := range p.Items {
		ids.WriteString(item.ID)
		ids.WriteByte(0)
	}
	digest := sha256.Sum256([]byte(ids.String()))
	hash := hex.EncodeToString(digest[:])
	if p.After == "" || p.After == before || p.After == g.AnchorCursor || hash == g.AnchorPage || hash == g.LastPage {
		return ErrPagination
	}
	g.LastPage = hash
	if g.Power == 0 {
		g.Power = 1
		g.AnchorCursor = p.After
		g.AnchorPage = hash
		return nil
	}
	g.Distance++
	if g.Distance == g.Power {
		g.AnchorCursor = p.After
		g.AnchorPage = hash
		g.Distance = 0
		if g.Power < 1<<63 {
			g.Power *= 2
		}
	}
	return nil
}
func (g *WalkGuard) restart() error {
	invalidations := g.Invalidations + 1
	if invalidations > 2 {
		return ErrPagination
	}
	*g = WalkGuard{Invalidations: invalidations}
	return nil
}
