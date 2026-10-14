package store_test

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/filenamediscovery"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"testing"
)

type filenamePersonAdapter struct{ s *store.Store }

func (a filenamePersonAdapter) GetPersonContext(ctx context.Context, id int64) (*store.Person, error) {
	return a.s.FilenamePersonContext(ctx, id)
}

// This catches separate snapshots, nested write checkout during resolution,
// stale person bindings, stale names and provenance from another authority era.
func TestFilenameSuggestionSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f, ids := filenameCandidateFixture(t, []string{"report.pdf"})
	s := f.Store
	first, err := s.EnsureParticipant("first@example.test", "First Example", "example.test")
	require.NoError(err)
	second, err := s.EnsureParticipant("second@example.test", "Second Example", "example.test")
	require.NoError(err)
	person, _, err := s.CreatePersonFromParticipant(first)
	require.NoError(err)
	var message int64
	require.NoError(s.DB().QueryRow(s.Rebind(`SELECT message_id FROM attachments WHERE id=?`), ids[0]).Scan(&message))
	require.NoError(s.ReplaceMessageRecipients(message, "to", []int64{first}, []string{"First Example"}))
	// Recipient writes rederive account attribution. Set the synthetic known
	// attribution only after that production write has completed.
	_, err = s.DB().Exec(s.Rebind(`UPDATE messages SET account_address='first@example.test',account_path='inbound' WHERE id=?`), message)
	require.NoError(err)
	var account string
	require.NoError(s.DB().QueryRow(s.Rebind(`SELECT account_address FROM messages WHERE id=?`), message).Scan(&account))
	require.Equal("first@example.test", account)
	filenameReady(t, s)
	writer, err := store.OpenForTest(filenameLocation(t, s))
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(writer.Close()) })
	s.DB().SetMaxOpenConns(1)
	ctx, release, err := s.BeginReadSnapshotContext(t.Context())
	require.NoError(err)
	defer release()
	resolved, err := resolver.Resolve(ctx, filenamePersonAdapter{s}, resolver.Reference{Kind: resolver.ReferencePerson, ID: person.ID}, nil)
	require.NoError(err)
	assert.Equal([]int64{first}, resolved.Scope.ParticipantIDs)
	q, err := filenamediscovery.ParseQuery("report.pdf")
	require.NoError(err)
	oldSet, err := s.FilenameCandidatesContext(ctx, q, store.FilenameSuggestionScope{})
	require.NoError(err)
	changed := make(chan error, 1)
	go func() {
		tx, err := writer.DB().BeginTx(t.Context(), nil)
		if err != nil {
			changed <- err
			return
		}
		defer func() { _ = tx.Rollback() }()
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`UPDATE attachments SET filename='renamed.pdf' WHERE id=?`, []any{ids[0]}},
			{`UPDATE messages SET account_address='second@example.test' WHERE id=?`, []any{message}},
			{`DELETE FROM person_participants WHERE person_id=?`, []any{person.ID}},
			{`INSERT INTO person_participants(person_id,participant_id) VALUES(?,?)`, []any{person.ID, second}},
			{`UPDATE message_recipients SET participant_id=? WHERE message_id=?`, []any{second, message}},
		} {
			if _, err = tx.ExecContext(t.Context(), writer.Rebind(statement.sql), statement.args...); err != nil {
				changed <- err
				return
			}
		}
		changed <- tx.Commit()
	}()
	require.NoError(<-changed)
	// Reconcile through a physically separate writer while the original reader
	// holds its authority era. Revision is also part of that original snapshot.
	filenameReady(t, writer)
	again, err := resolver.Resolve(ctx, filenamePersonAdapter{s}, resolver.Reference{Kind: resolver.ReferencePerson, ID: person.ID}, nil)
	require.NoError(err)
	assert.Equal(resolved, again)
	still, err := s.FilenameCandidatesContext(ctx, q, store.FilenameSuggestionScope{})
	require.NoError(err)
	assert.Equal(oldSet, still)
	oldRows, err := s.FilenameSuggestionRowsContext(ctx, oldSet.IDs, store.FilenameSuggestionScope{AccountScopes: []search.AccountScope{{Addresses: []string{"first@example.test"}}}, Person: &resolved.Scope})
	require.NoError(err)
	require.Len(oldRows, 1)
	assert.Equal("report.pdf", oldRows[0].File.Filename)
	require.NotNil(oldRows[0].PersonProvenance)
	assert.Equal([]int64{first}, oldRows[0].PersonProvenance.ParticipantIDs)
	release()
	fresh, freshRelease, err := s.BeginReadSnapshotContext(t.Context())
	require.NoError(err)
	defer freshRelease()
	next, err := resolver.Resolve(fresh, filenamePersonAdapter{s}, resolver.Reference{Kind: resolver.ReferencePerson, ID: person.ID}, nil)
	require.NoError(err)
	assert.Equal([]int64{second}, next.Scope.ParticipantIDs)
	renamed, err := filenamediscovery.ParseQuery("renamed.pdf")
	require.NoError(err)
	newSet, err := s.FilenameCandidatesContext(fresh, renamed, store.FilenameSuggestionScope{})
	require.NoError(err)
	assert.Equal(ids, newSet.IDs)
	assert.NotEqual(oldSet.IndexRevision, newSet.IndexRevision)
	newRows, err := s.FilenameSuggestionRowsContext(fresh, newSet.IDs, store.FilenameSuggestionScope{AccountScopes: []search.AccountScope{{Addresses: []string{"second@example.test"}}}, Person: &next.Scope})
	require.NoError(err)
	require.Len(newRows, 1)
	assert.Equal("renamed.pdf", newRows[0].File.Filename)
	require.NotNil(newRows[0].PersonProvenance)
	assert.Equal([]int64{second}, newRows[0].PersonProvenance.ParticipantIDs)
	mismatched, err := s.FilenameSuggestionRowsContext(fresh, newSet.IDs, store.FilenameSuggestionScope{AccountScopes: []search.AccountScope{{Addresses: []string{"first@example.test"}}}, Person: &next.Scope})
	require.NoError(err)
	assert.Empty(mismatched)
}
