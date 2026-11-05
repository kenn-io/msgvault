package timeline

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildRejectsIgnoredSelectors(t *testing.T) {
	f := newTimelineFixture(t)
	for _, tc := range []struct {
		name    string
		request Request
		options []string
	}{
		{"relationship date", Request{With: "email:alice@example.com", Date: "2026-10-22"}, []string{"with", "date"}},
		{"anchor date", Request{Around: "2026-10-22", Date: "2026-10-22"}, []string{"around", "date"}},
		{"anchor malformed date", Request{Around: "2026-10-22", Date: "bad"}, []string{"around", "date"}},
		{"day anchor", Request{Mode: ModeDay, Around: "msg:999999"}, []string{"mode=day", "around"}},
		{"conversation without anchor", Request{Conversation: true}, []string{"conversation", "around"}},
		{"anchor mode conversation without anchor", Request{Mode: ModeAnchor, Conversation: true}, []string{"conversation", "around"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.request
			req.Scope = Scope{Owner: true}
			got, err := Build(t.Context(), f.src, req)
			require.ErrorIs(t, err, ErrInvalidRequest)
			assert.Nil(t, got)
			for _, option := range tc.options {
				assert.Contains(t, err.Error(), option)
			}
		})
	}
}

func TestBuildAllowsCombinedAnchorNarrowing(t *testing.T) {
	f := newTimelineFixture(t)
	id := f.add(f.now, false)
	f.project()
	req := anchorRequest(messageRef(id))
	req.With = "email:alice@example.com"
	req.Conversation = true
	got, err := Build(t.Context(), f.src, req)
	require.NoError(t, err)
	require.Len(t, got.History, 1)
	assert.Equal(t, messageRef(id), got.History[0].AnchorRef)
}
