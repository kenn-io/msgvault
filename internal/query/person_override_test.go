package query

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func personOverrideFixture(t *testing.T) (*TestDataBuilder, int64, int64) {
	t.Helper()
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("user@example.com", "gmail")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice Observed")
	alias := b.AddParticipant("alice.alias@example.com", "example.com", "Alice Alias")
	b.AddParticipantIdentifier(alias, "username", "alice_handle", "Alice Handle", false)
	b.LinkCluster(alice, alias)
	m := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 10, SentAt: time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)})
	b.AddFrom(m, alice, "Alice Observed")
	return b, alice, alias
}

func addPersonOverride(b *TestDataBuilder, participantID, personID int64, name *string) {
	value := "NULL::VARCHAR"
	if name != nil {
		value = sqlStr(*name)
	}
	b.personDisplayNames = append(b.personDisplayNames, fmt.Sprintf("(%d::BIGINT, %d::BIGINT, %s)", participantID, personID, value))
}

func TestSearchPeopleAppliesPersonDisplayNameOverride(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b, alice, alias := personOverrideFixture(t)
	name := "Alice Curated"
	addPersonOverride(b, alias, 10, &name)
	e := b.BuildEngine()
	rows, err := e.SearchPeople(context.Background(), PersonSearchRequest{})
	requirements.NoError(err)
	requirements.Len(rows.Rows, 1)
	detail, err := e.GetPerson(context.Background(), alice, Context{}, []int64{alice, alias})
	requirements.NoError(err)
	for _, row := range []*PersonSummary{&rows.Rows[0], detail} {
		requirements.NotNil(row)
		assertions.Equal(name, row.DisplayLabel)
		assertions.Equal("Alice Observed", row.DisplayName)
		assertions.False(row.PartialLabel)
		t.Logf("display_label=%q display_name=%q partial_label=%v", row.DisplayLabel, row.DisplayName, row.PartialLabel)
	}
}

func TestPersonOverrideLabelMatchesAcrossIndexedAndLegacyPaths(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b, alice, alias := personOverrideFixture(t)
	name := "Alice Curated"
	addPersonOverride(b, alias, 10, &name)
	e := b.BuildEngine()
	indexed, err := e.SearchPeople(context.Background(), PersonSearchRequest{})
	requirements.NoError(err)
	legacy, err := e.searchPeopleLegacy(context.Background(), PersonSearchRequest{}, nil, nil)
	requirements.NoError(err)
	requirements.Len(indexed.Rows, 1)
	requirements.Len(legacy.Rows, 1)
	assertions.Equal(indexed.Rows[0].DisplayLabel, legacy.Rows[0].DisplayLabel)
	exact, err := e.searchPeopleLegacy(context.Background(), PersonSearchRequest{}, &alice, []int64{alice, alias})
	requirements.NoError(err)
	requirements.Len(exact.Rows, 1)
	assertions.Equal(name, exact.Rows[0].DisplayLabel)
	t.Logf("indexed=%q legacy=%q exact=%q", indexed.Rows[0].DisplayLabel, legacy.Rows[0].DisplayLabel, exact.Rows[0].DisplayLabel)
}

func TestSearchPeopleMatchesCuratedNameAndPreservesObservedAliases(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b, _, alias := personOverrideFixture(t)
	name := "Alice Curated"
	addPersonOverride(b, alias, 10, &name)
	e := b.BuildEngine()
	for _, term := range []string{name, "Alice Observed", "alice.alias@example.com", "alice_handle"} {
		indexed, err := e.SearchPeople(context.Background(), PersonSearchRequest{Query: term})
		requirements.NoError(err)
		legacy, err := e.searchPeopleLegacy(context.Background(), PersonSearchRequest{Query: term}, nil, nil)
		requirements.NoError(err)
		assertions.Len(indexed.Rows, 1, term)
		assertions.Len(legacy.Rows, 1, term)
		t.Logf("query=%q indexed=%d legacy=%d", term, len(indexed.Rows), len(legacy.Rows))
	}
}

func TestRelationshipsListUsesPersonDisplayName(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b, _, alias := personOverrideFixture(t)
	name := "Alice Curated"
	addPersonOverride(b, alias, 10, &name)
	e := b.BuildEngine()
	result, err := e.Relationships(context.Background(), RelationshipsRequest{Limit: 10, ShowAll: true})
	requirements.NoError(err)
	requirements.Len(result.Rows, 1)
	assertions.Equal(name, result.Rows[0].DisplayLabel)
}

func TestPersonOverrideDeterministicAcrossConflictingBindings(t *testing.T) {
	for _, samePerson := range []bool{false, true} {
		t.Run(strconv.FormatBool(samePerson), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			b, alice, alias := personOverrideFixture(t)
			first, second := "Alice First", "Alice Second"
			personID := int64(20)
			if samePerson {
				personID = 10
				first = second
			}
			addPersonOverride(b, alice, personID, &first)
			addPersonOverride(b, alias, 10, &second)
			e := b.BuildEngine()
			for i := range 2 {
				if i > 0 {
					e = b.BuildEngine()
				}
				indexed, err := e.SearchPeople(context.Background(), PersonSearchRequest{})
				requirements.NoError(err)
				legacy, err := e.searchPeopleLegacy(context.Background(), PersonSearchRequest{}, nil, nil)
				requirements.NoError(err)
				requirements.Len(indexed.Rows, 1)
				requirements.Len(legacy.Rows, 1)
				assertions.Equal(second, indexed.Rows[0].DisplayLabel)
				assertions.Equal(second, legacy.Rows[0].DisplayLabel)
			}
		})
	}
}

func TestPersonOverrideLeavesUnboundAndBlankNameLabelsUnchanged(t *testing.T) {
	for _, mode := range []string{"unbound", "null", "blank"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			b := NewTestDataBuilder(t)
			source := b.AddSourceWithType("user@example.com", "gmail")
			names := []string{"Alice Observed", "+15550100001", "alice@example.com", "alice_handle", "Unknown person #5"}
			for i := range names {
				id := b.AddParticipant("", "", "")
				switch i {
				case 0:
					b.participants[i].DisplayName = names[i]
				case 1:
					b.participants[i].PhoneNumber = names[i]
				case 2:
					b.participants[i].Email = names[i]
				case 3:
					b.AddParticipantIdentifier(id, "username", names[i], names[i], true)
				}
				if mode == "null" {
					addPersonOverride(b, id, id, nil)
				}
				if mode == "blank" {
					blank := "   "
					addPersonOverride(b, id, id, &blank)
				}
				m := b.AddMessage(MessageOpt{SourceID: source, ConversationID: id, SentAt: time.Date(2026, 7, 10, 9, i, 0, 0, time.UTC)})
				b.AddFrom(m, id, "")
			}
			e := b.BuildEngine()
			result, err := e.SearchPeople(context.Background(), PersonSearchRequest{})
			requirements.NoError(err)
			requirements.Len(result.Rows, len(names))
			for _, row := range result.Rows {
				assertions.Equal(names[row.ID-1], row.DisplayLabel)
				assertions.Equal(row.ID != 1, row.PartialLabel)
				var oldLabel string
				requirements.NoError(e.db.QueryRow("SELECT "+sqlPersonDisplayLabelExpr("NULLIF(TRIM(p.display_name), '')", "p")+" FROM participants p WHERE id = ?", row.ID).Scan(&oldLabel))
				assertions.Equal(oldLabel, row.DisplayLabel)
				legacy, err := e.searchPeopleLegacy(context.Background(), PersonSearchRequest{}, &row.ID, nil)
				requirements.NoError(err)
				requirements.Len(legacy.Rows, 1)
				assertions.Equal(oldLabel, legacy.Rows[0].DisplayLabel)
				assertions.Equal(row.PartialLabel, legacy.Rows[0].PartialLabel)
				t.Logf("%s label=%q partial_label=%v", mode, row.DisplayLabel, row.PartialLabel)
			}
		})
	}
}

func TestPersonProfileRevisionComesFromStoreNotCache(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "profile.db"))
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	id, err := st.EnsureParticipantByIdentifier("email", "alice@example.com", "Alice Observed")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(id)
	requirements.NoError(err)
	b, alice, alias := personOverrideFixture(t)
	name := "Alice Cached"
	addPersonOverride(b, alias, person.ID, &name)
	e := b.BuildEngine()
	for _, name := range []string{"Alice Updated", "Alice Current"} {
		person, err = st.UpdatePersonDisplayName(person.ID, person.Revision, &name)
		requirements.NoError(err)
	}
	cached, err := e.GetPerson(context.Background(), alice, Context{}, []int64{alice, alias})
	requirements.NoError(err)
	requirements.NotNil(cached)
	assertions.Nil(cached.Profile)
	assertions.Equal("Alice Cached", cached.DisplayLabel)
	live, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(person.Revision, live.Revision)
	assertions.Equal(int64(3), live.Revision)
	t.Logf("cached label=%q cached profile=%v live revision=%d", cached.DisplayLabel, cached.Profile, live.Revision)
}
