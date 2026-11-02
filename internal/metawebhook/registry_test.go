package metawebhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRegistryAllowlistAndDuplicatePanics(t *testing.T) {
	r := NewRegistry()
	h := func(context.Context, string, []Item) []ItemOutcome { return nil }
	r.Register("page", "feed", h)
	_, ok := r.Lookup("page", "feed")
	assert.True(t, ok)
	_, ok = r.Lookup("page", "messages")
	assert.False(t, ok)
	assert.Panics(t, func() { r.Register("page", "feed", h) })
	assert.Panics(t, func() { r.Register("page", "unknown", h) })
	assert.Panics(t, func() { r.Register("other", "feed", h) })
	assert.Panics(t, func() { r.Register("instagram", "comments", nil) })
	r.Register("instagram", "comments", h)
}
func TestSettleAggregation(t *testing.T) {
	for _, tt := range []struct {
		name  string
		out   []ItemOutcome
		state State
		count bool
	}{
		{"all applied", []ItemOutcome{{Class: Applied}, {Class: Ignored}}, StateDone, false},
		{"mixed held", []ItemOutcome{{Class: Applied}, {Class: Unhandled}}, StateHeld, false},
		{"retry", []ItemOutcome{{Class: Unhandled}, {Class: Retry, ErrorClass: "permission_missing", CountAttempt: true}}, StatePending, true},
		{"busy", []ItemOutcome{{Class: Retry, ErrorClass: "sync_active"}}, StatePending, false},
		{"invalid", []ItemOutcome{{Class: Invalid}}, StateHeld, false}, {"unrouted", []ItemOutcome{{Class: Unrouted}}, StateHeld, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			items := make([]Item, len(tt.out))
			for i := range items {
				items[i] = Item{Object: "page", Field: "feed"}
			}
			got := Settle(tt.out, items)
			assert.Equal(t, tt.state, got.State)
			assert.Equal(t, tt.count, got.CountAttempt)
			n := 0
			for _, count := range got.Outcomes {
				n += count
			}
			assert.Equal(t, len(tt.out), n)
		})
	}
}
