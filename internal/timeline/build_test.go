package timeline

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildRejectsInvalidRequests(t *testing.T) {
	f := newTimelineFixture(t)
	for _, tc := range []struct {
		name    string
		request Request
		options []string
		want    error
	}{
		{"relationship date", Request{With: "email:alice@example.com", Date: "2026-10-22"}, []string{"with", "date"}, ErrInvalidRequest},
		{"anchor date", Request{Around: "2026-10-22", Date: "2026-10-22"}, []string{"around", "date"}, ErrInvalidRequest},
		{"anchor malformed date", Request{Around: "2026-10-22", Date: "bad"}, []string{"around", "date"}, ErrInvalidRequest},
		{"day anchor", Request{Mode: ModeDay, Around: "msg:999999"}, []string{"mode=day", "around"}, ErrInvalidRequest},
		{"conversation without anchor", Request{Conversation: true}, []string{"conversation", "around"}, ErrInvalidRequest},
		{"anchor mode conversation without anchor", Request{Mode: ModeAnchor, Conversation: true}, []string{"conversation", "around"}, ErrInvalidRequest},
		{"relationship range", Request{With: "email:alice@example.com", From: "2026-10-21", To: "2026-10-22"}, nil, ErrInvalidRequest},
		{"relationship days", Request{With: "email:alice@example.com", Days: 7}, nil, ErrInvalidRequest},
		{"anchor days", Request{Around: "2026-10-22", Days: 7}, nil, ErrInvalidRequest},
		{"anchor range", Request{Around: "2026-10-22", From: "2026-10-21", To: "2026-10-22"}, nil, ErrInvalidRequest},
		{"brief days", Request{Brief: true, Days: 7}, nil, ErrInvalidRequest},
		{"too many days", Request{Days: 32}, nil, ErrWindowTooLarge},
		{"too many neighbours", Request{Before: 51}, nil, ErrInvalidRequest},
		{"negative neighbours", Request{After: -1}, nil, ErrInvalidRequest},
		{"small budget", Request{Budget: 2000}, nil, ErrInvalidRequest},
		{"invalid mode", Request{Mode: "invalid"}, nil, ErrInvalidRequest},
		{"invalid date", Request{Date: "bad"}, nil, ErrInvalidRequest},
		{"invalid tier", Request{Tiers: []int{4}}, nil, ErrInvalidRequest},
		{"date and days", Request{Date: "2026-10-22", Days: 4}, nil, ErrInvalidRequest},
		{"incomplete range", Request{From: "2026-10-22"}, nil, ErrInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.request
			req.Scope = Scope{Owner: true}
			got, err := Build(t.Context(), f.src, req)
			require.ErrorIs(t, err, tc.want)
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
