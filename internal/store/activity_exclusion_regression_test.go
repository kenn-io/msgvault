package store_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
)

func TestActivityMixedConversationExcludesNoiseBeforeDisplay(t *testing.T) {
	for _, noise := range []string{"list", "automated sender"} {
		t.Run(noise, func(t *testing.T) {
			requirements := require.New(t)
			f := newTimelineActivityFixture(t)
			at := timelineContactQuery(f).Now
			personal := f.add(t, f.ConvID, f.Source.ID, at, false, "")
			if noise == "automated sender" {
				f.peer = f.EnsureParticipant("noreply@example.com", "Automated sender", "example.com")
			}
			excluded := f.add(t, f.ConvID, f.Source.ID, at.Add(time.Hour), false, "")
			if noise == "list" {
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='list.example.com' WHERE id=?`), excluded)
				requirements.NoError(err)
			}
			projectTimelineActivity(t, f, "UTC")
			for _, include := range []bool{false, true} {
				t.Run(strconv.FormatBool(include), func(t *testing.T) {
					assertions, requirements := assert.New(t), require.New(t)
					q := timelineBurstQuery()
					q.Filters.IncludeNoise = include
					page, err := f.Store.ListActivityBurstsContext(t.Context(), q)
					requirements.NoError(err)
					requirements.Len(page.Bursts, 1)
					messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, Limit: 10, Filters: q.Filters})
					requirements.NoError(err)
					ids := make([]int64, len(messages))
					for i, m := range messages {
						ids[i] = m.MessageID
					}
					if include {
						assertions.Equal(excluded, page.Bursts[0].AnchorMessageID)
						assertions.Equal([]int64{excluded, personal}, page.Bursts[0].LatestMessageIDs)
						assertions.Equal([]int64{personal, excluded}, ids)
						assertions.Zero(page.NoiseOmitted)
					} else {
						assertions.Equal(personal, page.Bursts[0].AnchorMessageID)
						assertions.Equal([]int64{personal}, page.Bursts[0].LatestMessageIDs)
						assertions.Equal([]int64{personal}, ids)
						assertions.Equal(1, page.NoiseOmitted)
						assertions.Equal(at, page.Bursts[0].LastAt.UTC())
					}
				})
			}
		})
	}
}

func TestActivityUnicodeAddressExclusions(t *testing.T) {
	for _, identifier := range []bool{false, true} {
		t.Run(strconv.FormatBool(identifier), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			if identifier {
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address=NULL WHERE id=?`), f.peer)
				requirements.NoError(err)
				requirements.NoError(f.Store.SetParticipantIdentifier(f.peer, "email", "BÜRO@example.com"))
			} else {
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address='BÜRO@example.com' WHERE id=?`), f.peer)
				requirements.NoError(err)
			}
			q := timelineContactQuery(f)
			f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
			projectTimelineActivity(t, f, "UTC")
			q.Filters.Exclusions = persontiers.DefaultConfig()
			q.Filters.Exclusions.ExcludeAddressPatterns = []string{"büro"}
			aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Zero(aggregate.Count)
			bq := timelineBurstQuery()
			bq.Filters = q.Filters
			page, err := f.Store.ListActivityBurstsContext(t.Context(), bq)
			requirements.NoError(err)
			assertions.Empty(page.Bursts)
			assertions.Equal(1, page.NoiseOmitted)
		})
	}
}

func TestResolvePersonReferenceUnicodeCase(t *testing.T) {
	for _, field := range []string{"email", "identifier", "participant name", "saved name"} {
		t.Run(field, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			kind, value := store.PersonRefKind("name"), "büro"
			var personID int64
			switch field {
			case "email":
				kind, value = "email", "büro@example.com"
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address='BÜRO@example.com' WHERE id=?`), f.peer)
				requirements.NoError(err)
			case "identifier":
				kind, value = "email", "büro@example.com"
				requirements.NoError(f.Store.SetParticipantIdentifier(f.peer, "email", "BÜRO@example.com"))
			case "participant name":
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET display_name='BÜRO' WHERE id=?`), f.peer)
				requirements.NoError(err)
			case "saved name":
				person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				requirements.NoError(err)
				personID = person.ID
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE persons SET display_name='BÜRO' WHERE id=?`), personID)
				requirements.NoError(err)
			}
			got, err := f.Store.ResolvePersonReferenceContext(t.Context(), kind, value, nil, 10)
			requirements.NoError(err)
			requirements.Len(got, 1)
			assertions.Equal(personID, got[0].PersonID)
			assertions.Equal([]int64{f.peer}, got[0].ParticipantIDs)
		})
	}
}
