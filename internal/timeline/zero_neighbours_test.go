package timeline

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestNormalizeExplicitZeroNeighbours(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		before, after         int
		beforeSet, afterSet   bool
		wantBefore, wantAfter int
	}{
		{"omitted", 0, 0, false, false, 5, 5},
		{"both-zero", 0, 0, true, true, 0, 0},
		{"before-zero", 0, 3, true, false, 0, 3},
		{"after-zero", 2, 0, false, true, 2, 0},
		{"explicit-defaults", 5, 5, true, true, 5, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			req := Request{Around: "msg:12", Before: tc.before, After: tc.after, BeforeSet: tc.beforeSet, AfterSet: tc.afterSet}
			got, err := normalizeRequest(req)
			requirements.NoError(err)
			assertions.Equal(tc.wantBefore, got.Before)
			assertions.Equal(tc.wantAfter, got.After)
			twice, err := normalizeRequest(got)
			requirements.NoError(err)
			assertions.Equal(got, twice)
		})
	}
}

func TestBuildExplicitZeroNeighbours(t *testing.T) {
	f := newTimelineFixture(t)
	ids := []int64{}
	refs := []string{}
	for i := range 3 {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("zero-neighbours-%d", i), "Zero neighbours")
		require.NoError(t, err)
		refs = append(refs, fmt.Sprintf("conv:%d", conv))
		ids = append(ids, f.add(f.now.Add(time.Duration(i-1)*time.Hour), false, func(m *store.Message) { m.ConversationID = conv }))
	}
	f.project()
	for _, tc := range []struct {
		before, after int
		indexes       []int
	}{
		{0, 0, []int{1}}, {0, 1, []int{1, 2}}, {1, 0, []int{0, 1}},
	} {
		t.Run(fmt.Sprintf("before=%d/after=%d", tc.before, tc.after), func(t *testing.T) {
			requirements := require.New(t)
			req := anchorRequest(messageRef(ids[1]))
			req.Before, req.After = tc.before, tc.after
			req.BeforeSet, req.AfterSet = true, true
			got, err := Build(t.Context(), f.src, req)
			requirements.NoError(err)
			requirements.NotNil(got.Anchor)
			requirements.False(got.Anchor.Filtered)
			want := []string{}
			for _, i := range tc.indexes {
				want = append(want, refs[i])
			}
			gotRefs := []string{}
			for _, e := range got.History {
				gotRefs = append(gotRefs, e.Ref)
			}
			assert.Equal(t, want, gotRefs)
		})
	}
}

func TestCursorBindsEffectiveZeroNeighbours(t *testing.T) {
	requirements := require.New(t)
	b := cursorBinding()
	b.Request = anchorRequest("msg:12")
	omitted, err := EncodeCursor(b, map[string]string{"history": "key"})
	requirements.NoError(err)
	b.Request.Before, b.Request.After = 5, 5
	b.Request.BeforeSet, b.Request.AfterSet = true, true
	_, err = DecodeCursor(omitted, b)
	requirements.NoError(err, "explicit defaults and omitted defaults have identical effective counts")
	for _, before := range []bool{false, true} {
		b.Request = anchorRequest("msg:12")
		if before {
			b.Request.BeforeSet = true
		} else {
			b.Request.AfterSet = true
		}
		_, err = DecodeCursor(omitted, b)
		requirements.ErrorIs(err, ErrCursorExpired)
		zero, err := EncodeCursor(b, map[string]string{"history": "key"})
		requirements.NoError(err)
		_, err = DecodeCursor(zero, b)
		requirements.NoError(err)
		normalized, err := normalizeRequest(b.Request)
		requirements.NoError(err)
		b.Request = normalized
		_, err = DecodeCursor(zero, b)
		requirements.NoError(err, "Build's normalized request retains the same zero binding")
		b.Request = anchorRequest("msg:12")
		_, err = DecodeCursor(zero, b)
		requirements.ErrorIs(err, ErrCursorExpired)
	}
}
