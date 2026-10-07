package kataissues_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/taskclient"
)

func TestContextRecoversSavedQuotesForChangedPassages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	_, err := f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE message_bodies SET body_text=? WHERE message_id=?"), "Bring the budget.\n> Review the totals.\n\nSend it Friday.\n", f.message)
	require.NoError(err)
	bodyQuote := f.evidence(t, 0)
	commentQuote := f.evidence(t, 18)
	created, err := f.service.Create(t.Context(), "key-saved-quotes", kataissues.CreateInput{Title: "Send the budget", Evidence: []kataevidence.Reference{bodyQuote.Reference}})
	require.NoError(err)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{commentQuote.Reference})
	require.NoError(err)
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE message_bodies SET body_text=? WHERE message_id=?"), "Send it Monday instead.", f.message)
	require.NoError(err)

	read, err := f.service.Context(t.Context(), created.Issue.QualifiedRef, 0)
	require.NoError(err)
	require.Len(read.Passages, 2)
	for i, quote := range []kataevidence.Evidence{bodyQuote, commentQuote} {
		assert.Equal(kataevidence.Changed, read.Passages[i].State)
		assert.Equal(quote.Passage, read.Passages[i].Evidence.Passage)
		assert.Equal(quote.Excerpt, read.Passages[i].SavedQuote)
		assert.Empty(read.Passages[i].Evidence.Excerpt)
	}

	f = newFixture(t)
	bodyQuote = f.evidence(t, 0)
	commentQuote = f.evidence(t, 1500)
	created, err = f.service.Create(t.Context(), "key-pending-quote", kataissues.CreateInput{Title: "Send the budget", Evidence: []kataevidence.Reference{bodyQuote.Reference}})
	require.NoError(err)
	f.kata.failComment.Store(true)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{commentQuote.Reference})
	require.Error(err)
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE message_bodies SET body_text=body_text||? WHERE message_id=?"), " Signature added on re-sync.", f.message)
	require.NoError(err)
	resynced := f.evidence(t, 1500)
	require.Equal(commentQuote.Passage, resynced.Passage)
	require.NotEqual(commentQuote.ID, resynced.ID)
	f.kata.failComment.Store(true)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{resynced.Reference})
	require.Error(err)
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?"), f.message)
	require.NoError(err)

	read, err = f.service.Context(t.Context(), created.Issue.QualifiedRef, 0)
	require.NoError(err)
	require.Len(read.Passages, 2)
	assert.Empty(read.Issue.CommentBodies)
	for i, quote := range []kataevidence.Evidence{bodyQuote, commentQuote} {
		assert.Equal(kataevidence.Unavailable, read.Passages[i].State)
		assert.Equal(quote.Passage, read.Passages[i].Evidence.Passage)
		assert.Equal(quote.Excerpt, read.Passages[i].SavedQuote)
		assert.Empty(read.Passages[i].Evidence.Excerpt)
	}
}

func TestContextReportsEachPassageOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	passage := f.evidence(t, 1500)
	created, err := f.service.Create(t.Context(), "key-context", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{passage.Reference}})
	require.NoError(err)
	second := f.store.CreateMessage("second-message")
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("INSERT INTO message_bodies (message_id,body_text) VALUES (?,?)"), second, "Bring the slides on Monday.")
	require.NoError(err)
	other, err := kataevidence.New(f.store.Store).Prepare(t.Context(), []kataevidence.Selector{{Kind: "message", MessageID: second, MaxChars: new(kataevidence.MaxChars)}})
	require.NoError(err)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{other[0].Reference})
	require.NoError(err)

	// A re-sync past the passage records it again under a new body hash.
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE message_bodies SET body_text=body_text||? WHERE message_id=?"), " Signature added on re-sync.", f.message)
	require.NoError(err)
	resynced := f.evidence(t, 1500)
	require.Equal(passage.Passage, resynced.Passage)
	require.NotEqual(passage.ID, resynced.ID)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{resynced.Reference})
	require.NoError(err)
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?"), second)
	require.NoError(err)

	read, err := f.service.Context(t.Context(), created.Issue.QualifiedRef, 0)
	require.NoError(err)
	assert.Equal(created.Issue.UID, read.Issue.UID)
	assert.Zero(read.NextOffset)
	require.Len(read.Passages, 2)
	assert.Equal(kataevidence.Available, read.Passages[0].State)
	assert.Equal(resynced.ID, read.Passages[0].Evidence.ID)
	assert.Equal(string([]rune(f.evidence(t, 1000).Excerpt)[:kataevidence.ContextRunes]), read.Passages[0].Before)
	assert.Equal(kataevidence.Unavailable, read.Passages[1].State)
	assert.Equal(other[0].Passage, read.Passages[1].Evidence.Passage)
}

func TestContextPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	refs := make([]kataevidence.Reference, 0, kataissues.ContextPageSize+1)
	for i := range kataissues.ContextPageSize + 1 {
		refs = append(refs, f.evidence(t, i*100).Reference)
	}
	created, err := f.service.Create(t.Context(), "key-pages", kataissues.CreateInput{Title: "Send the revised budget", Evidence: refs})
	require.NoError(err)

	first, err := f.service.Context(t.Context(), created.Issue.QualifiedRef, 0)
	require.NoError(err)
	assert.Len(first.Passages, kataissues.ContextPageSize)
	assert.Equal(kataissues.ContextPageSize, first.NextOffset)
	last, err := f.service.Context(t.Context(), created.Issue.QualifiedRef, first.NextOffset)
	require.NoError(err)
	require.Len(last.Passages, 1)
	assert.Equal(kataevidence.ID(refs[kataissues.ContextPageSize]), last.Passages[0].Evidence.ID)
	assert.Zero(last.NextOffset)
	past, err := f.service.Context(t.Context(), created.Issue.QualifiedRef, kataissues.ContextPageSize+2)
	require.NoError(err)
	assert.Empty(past.Passages)
	assert.Zero(past.NextOffset)

	// An issue without msgvault evidence reads as just the issue.
	plain, _, err := f.service.Kata.CreateTaskReused(t.Context(), "example", "key-plain", taskclient.KataCreate{Title: "Plan the offsite"})
	require.NoError(err)
	read, err := f.service.Context(t.Context(), plain.QualifiedRef, 0)
	require.NoError(err)
	assert.Equal(plain.UID, read.Issue.UID)
	assert.Empty(read.Passages)
}
