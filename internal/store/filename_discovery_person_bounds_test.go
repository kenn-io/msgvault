package store_test

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"strconv"
	"testing"
)

func filenamePersonFixture(t *testing.T, count int) (*storetest.Fixture, int64, []int64) {
	t.Helper()
	f := storetest.New(t)
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	var person int64
	require.NoError(t, tx.QueryRow(`INSERT INTO persons(vcard_uid,display_name) VALUES('filename-scope-person','Scope Example') RETURNING id`).Scan(&person))
	var ids []int64
	for i := range count {
		var id int64
		require.NoError(t, tx.QueryRow(f.Store.Rebind(`INSERT INTO participants(email_address,display_name) VALUES(?,?) RETURNING id`), fmt.Sprintf("member%03d@example.test", i), "Member Example").Scan(&id))
		_, err := tx.Exec(f.Store.Rebind(`INSERT INTO person_participants(person_id,participant_id) VALUES(?,?)`), person, id)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	require.NoError(t, tx.Commit())
	return f, person, ids
}

func TestFilenamePersonScopeBounds(t *testing.T) {
	for _, count := range []int{0, 64, 65} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			f, id, members := filenamePersonFixture(t, count)
			f.Store.DB().SetMaxOpenConns(1)
			ctx, release, err := f.Store.BeginReadSnapshotContext(t.Context())
			require.NoError(err)
			defer release()
			person, err := f.Store.FilenamePersonContext(ctx, id)
			if count == 65 {
				require.ErrorIs(err, store.ErrFilenameScopeTooLarge)
				assert.Nil(person)
				return
			}
			require.NoError(err)
			if count == 0 {
				assert.Empty(person.ParticipantIDs)
			} else {
				assert.Equal(members, person.ParticipantIDs)
			}
			assert.Equal("filename-scope-person", person.VCardUID)
			assert.Empty(person.CardDAVBindings)
			_, err = resolver.Resolve(ctx, filenamePersonAdapter{f.Store}, resolver.Reference{Kind: resolver.ReferencePerson, ID: id}, nil)
			if count == 0 {
				require.ErrorIs(err, resolver.ErrEmptyPopulation)
			} else {
				require.NoError(err)
			}
			_, err = f.Store.FilenamePersonContext(ctx, id+999)
			require.ErrorIs(err, store.ErrPersonNotFound)
		})
	}
}

// 64 messages ×64 participants ×one role are exactly4096 unique tuples.
// Duplicate envelope observations never add a tuple; one new role must reject
// all rows at4097 rather than silently truncate a person's mandatory scope.
func TestFilenamePersonScopeProvenanceBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f, _, members := filenamePersonFixture(t, 64)
	scope := personscope.Scope{ParticipantIDs: members, Directions: []personscope.Direction{personscope.ToPerson}}
	var ids, messages []int64
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	for i := range 64 {
		// Real archive rows use one synthetic conversation/source and actual FK roots.
		var message, id int64
		require.NoError(tx.QueryRow(f.Store.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type) VALUES(?,?,?,'email') RETURNING id`), f.Source.ID, f.ConvID, fmt.Sprintf("provenance-%d", i)).Scan(&message))
		require.NoError(tx.QueryRow(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,storage_path) VALUES(?,'report.pdf','') RETURNING id`), message).Scan(&id))
		ids = append(ids, id)
		messages = append(messages, message)
		for _, member := range members {
			_, err = tx.Exec(f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES(?,?,'to','alias@example.test')`), message, member)
			require.NoError(err)
		}
	}
	// Same role/participant/message but another envelope is an observation, not
	// an additional provenance tuple.
	_, err = tx.Exec(f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES(?,?,'to','other-alias@example.test')`), messages[0], members[0])
	require.NoError(err)
	require.NoError(tx.Commit())
	rows, err := f.Store.FilenameSuggestionRowsContext(t.Context(), ids, store.FilenameSuggestionScope{Person: &scope})
	require.NoError(err)
	require.Len(rows, 64)
	for _, row := range rows {
		require.NotNil(row.PersonProvenance)
		assert.Equal(members, row.PersonProvenance.ParticipantIDs)
		assert.Equal([]personscope.Role{personscope.RoleTo}, row.PersonProvenance.Roles)
	}
	// Sender plus envelope-from is still exactly ONE additional role tuple.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), members[0], messages[0])
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES(?,?,'from','member000@example.test')`), messages[0], members[0])
	require.NoError(err)
	overflow, err := f.Store.FilenameSuggestionRowsContext(t.Context(), ids, store.FilenameSuggestionScope{Person: &scope})
	require.ErrorIs(err, store.ErrFilenameScopeTooLarge)
	assert.Nil(overflow, "no partial authority rows on provenance overflow")
}
