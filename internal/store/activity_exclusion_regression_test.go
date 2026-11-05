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
			personalPeer := f.peer
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
			contact := timelineContactQuery(f)
			contact.Subject = store.ActivitySubject{ParticipantIDs: []int64{personalPeer, f.peer}}
			contact.Now = at.Add(2 * time.Hour)
			aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), contact)
			requirements.NoError(err)
			assert.Equal(t, 1, aggregate.Count)
			assert.Equal(t, &at, aggregate.LastAt)
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
	for _, tc := range []struct {
		name                      string
		identifier, saved, domain bool
	}{
		{name: "address"},
		{name: "identifier", identifier: true},
		{name: "custom domain unsaved", domain: true},
		{name: "custom domain saved", domain: true, saved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			if tc.domain {
				if tc.saved {
					_, _, err := f.Store.CreatePersonFromParticipant(f.peer)
					requirements.NoError(err)
				}
			} else if tc.identifier {
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address=NULL WHERE id=?`), f.peer)
				requirements.NoError(err)
				requirements.NoError(f.Store.SetParticipantIdentifier(f.peer, "email", "BÜRO@example.com"))
			} else {
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address='BÜRO@example.com' WHERE id=?`), f.peer)
				requirements.NoError(err)
			}
			q := timelineContactQuery(f)
			if tc.domain {
				id := f.add(t, f.ConvID, f.Source.ID, q.Now, true, "")
				bob := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer, bob}, []string{"Alice", "Bob"}))
			} else {
				f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
			}
			projectTimelineActivity(t, f, "UTC")
			bq := timelineBurstQuery()
			page, err := f.Store.ListActivityBurstsContext(t.Context(), bq)
			requirements.NoError(err)
			requirements.Len(page.Bursts, 1)
			q.Filters.Exclusions = persontiers.DefaultConfig()
			if tc.domain {
				q.Filters.Exclusions.ExcludeDomainPatterns = []string{"example.com"}
			} else {
				q.Filters.Exclusions.ExcludeAddressPatterns = []string{"büro"}
			}
			aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Zero(aggregate.Count)
			bq.Filters = q.Filters
			page, err = f.Store.ListActivityBurstsContext(t.Context(), bq)
			requirements.NoError(err)
			assertions.Empty(page.Bursts)
			assertions.Equal(1, page.NoiseOmitted)
			bq.Filters.IncludeNoise = true
			page, err = f.Store.ListActivityBurstsContext(t.Context(), bq)
			requirements.NoError(err)
			assertions.Len(page.Bursts, 1)
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
