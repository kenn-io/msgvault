package store_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
)

func TestActivityExcludesGoogleResourceSubdomains(t *testing.T) {
	for _, domain := range []struct {
		name     string
		excluded bool
	}{
		{"c.resource.calendar.google.com", true},
		{"floor.c.resource.calendar.google.com", true},
		{"resource.calendar.google.com.example.test", false},
	} {
		t.Run(domain.name, func(t *testing.T) {
			f := newTimelineActivityFixture(t)
			f.peer = f.EnsureParticipant("room@"+domain.name, "Synthetic room", domain.name)
			contact := timelineContactQuery(f)
			f.add(t, f.ConvID, f.Source.ID, contact.Now, false, "")
			projectTimelineActivity(t, f, "UTC")
			for _, defaults := range []bool{true, false} {
				t.Run("default-exclusions="+strconv.FormatBool(defaults), func(t *testing.T) {
					assertions, requirements := assert.New(t), require.New(t)
					filters := store.ActivityFilters{IncludeNoise: true, Exclusions: persontiers.DefaultConfig()}
					if !defaults {
						filters.Exclusions.ExcludeDomainPatterns = nil
					}
					wantCount, wantExclusion := 1, ""
					if defaults && domain.excluded {
						wantCount, wantExclusion = 0, "calendar_room"
					}
					q := timelineBurstQuery()
					q.Subject, q.Filters = contact.Subject, filters
					page, err := f.Store.ListActivityBurstsContext(t.Context(), q)
					requirements.NoError(err)
					requirements.Len(page.Bursts, 1)
					assertions.Equal(wantCount, page.Bursts[0].Inbound)
					found := false
					for _, cp := range page.Bursts[0].Counterparts {
						if cp.ParticipantID == f.peer {
							found = true
							assertions.Equal(wantExclusion, cp.Excluded)
							assertions.Equal(wantCount == 1, cp.Counted)
						}
					}
					assertions.True(found)
					cq := contact
					cq.Filters = filters
					aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), cq)
					requirements.NoError(err)
					assertions.Equal(wantCount, aggregate.Count)
					q.Filters.IncludeNoise = false
					quiet, err := f.Store.ListActivityBurstsContext(t.Context(), q)
					requirements.NoError(err)
					assertions.Len(quiet.Bursts, wantCount)
				})
			}
		})
	}
}
