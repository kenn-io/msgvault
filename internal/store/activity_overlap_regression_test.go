package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestOverlappingMailboxThreadsShareBurstAndInitiation(t *testing.T) {
	for _, shape := range []struct {
		name                     string
		partial                  bool
		count, inbound, outbound int
	}{
		{"ends one thread", false, 3, 2, 1},
		{"partial account", true, 2, 1, 1},
	} {
		t.Run(shape.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			q := timelineContactQuery(f)
			at := q.Now.Add(-2 * time.Hour)
			if shape.partial {
				at = q.Now.Add(-time.Hour)
			}
			first := f.add(t, f.ConvID, f.Source.ID, at, false, "<a@example.com>")
			shared := first
			sharedAt, sharedID, sharedOut := at, "<a@example.com>", false
			if !shape.partial {
				sharedAt, sharedID, sharedOut = at.Add(time.Hour), "<b@example.com>", true
				shared = f.add(t, f.ConvID, f.Source.ID, sharedAt, sharedOut, sharedID)
			}
			other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
			requirements.NoError(err)
			requirements.NoError(f.Store.AddAccountIdentity(other.ID, "owner@example.com", "manual"))
			conv, err := f.Store.EnsureConversation(other.ID, "overlap", "Overlapping thread")
			requirements.NoError(err)
			otherShared := f.add(t, conv, other.ID, sharedAt, sharedOut, sharedID)
			f.add(t, conv, other.ID, sharedAt.Add(time.Hour), !sharedOut, "<c@example.com>")
			if shape.partial {
				shared = otherShared
			}
			projectTimelineActivity(t, f, "UTC")
			page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
			requirements.NoError(err)
			requirements.Len(page.Bursts, 1)
			assertions.Equal(shape.inbound, page.Bursts[0].Inbound)
			assertions.Equal(shape.outbound, page.Bursts[0].Outbound)
			assertions.Equal(2, page.Bursts[0].CopyCounts[shared])
			if shape.partial {
				assertions.Equal(conv, page.Bursts[0].ConversationID)
			}
			aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Equal(shape.count, aggregate.Count)
			assertions.Equal(1, aggregate.InitiatedByContact)
			assertions.Zero(aggregate.InitiatedByOwner)
			scoped, err := f.Store.ListActivityBurstsContext(sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID}), timelineBurstQuery())
			requirements.NoError(err)
			requirements.Len(scoped.Bursts, 1)
			assertions.Equal(f.Source.ID, scoped.Bursts[0].SourceID)
			assertions.Equal(1, scoped.Bursts[0].Inbound)
			if shape.partial {
				assertions.Zero(scoped.Bursts[0].Outbound)
			} else {
				assertions.Equal(1, scoped.Bursts[0].Outbound)
			}
		})
	}
}
