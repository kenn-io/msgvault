package kataevidence_test

import (
	"cmp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPrepareWindowsAndResolveMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("evidence-message")
	text := strings.Repeat("a", 1000) + "é界🙂 send the budget by Friday"
	_, err := f.Store.DB().Exec(f.Store.Rebind("INSERT INTO message_bodies (message_id,body_text) VALUES (?,?)"), id, text)
	require.NoError(err)
	svc := kataevidence.New(f.Store)

	first, err := svc.Prepare(t.Context(), []kataevidence.Selector{{Kind: "message", MessageID: id, MaxChars: new(kataevidence.MaxChars)}})
	require.NoError(err)
	assert.Equal(strings.Repeat("a", 1000), first[0].Excerpt)
	assert.Equal(1000, first[0].NextRune)
	assert.Equal(kataevidence.PassageID(first[0].Reference, first[0].Excerpt), first[0].Passage, "clients key retries on the server's passage")

	later, err := svc.Prepare(t.Context(), []kataevidence.Selector{{Kind: "message", MessageID: id, StartRune: &first[0].NextRune, MaxChars: new(kataevidence.MaxChars)}})
	require.NoError(err)
	assert.Equal("é界🙂 send the budget by Friday", later[0].Excerpt)
	assert.Zero(later[0].NextRune, "the last window has no following passage")

	resolved, err := svc.ResolveAround(t.Context(), later[0].Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Available, resolved.State)
	assert.Equal(later[0].ID, resolved.Evidence.ID)
	assert.Equal(later[0].Excerpt, resolved.Evidence.Excerpt)

	around, err := svc.ResolveAround(t.Context(), later[0].Reference, kataevidence.ContextRunes)
	require.NoError(err)
	assert.Equal(strings.Repeat("a", kataevidence.ContextRunes), around.Before)
	around, err = svc.ResolveAround(t.Context(), first[0].Reference, kataevidence.ContextRunes)
	require.NoError(err)
	assert.Equal(later[0].Excerpt, around.After, "the window stops at the end of the text")

	// A hand-built range past the end of the text keeps a valid body hash.
	past := later[0].Reference
	payload := *past.Message
	payload.EndRune += 5
	past.Message = &payload
	resolved, err = svc.ResolveAround(t.Context(), past, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Changed, resolved.State)
	assert.Empty(resolved.Evidence.Excerpt)
	around, err = svc.ResolveAround(t.Context(), past, kataevidence.ContextRunes)
	require.NoError(err)
	assert.Empty(around.Before, "a changed reference shows no window")

	// The reader still shows messages deleted at their source, so they stay citable.
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?"), id)
	require.NoError(err)
	_, err = svc.Prepare(t.Context(), []kataevidence.Selector{{Kind: "message", MessageID: id, MaxChars: new(kataevidence.MaxChars)}})
	require.NoError(err)
	resolved, err = svc.ResolveAround(t.Context(), later[0].Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Available, resolved.State)

	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?"), id)
	require.NoError(err)
	resolved, err = svc.ResolveAround(t.Context(), first[0].Reference, 0)
	require.NoError(err)
	assert.Equal(kataevidence.Unavailable, resolved.State)
}

func TestPrepareMessageSelectors(t *testing.T) {
	f := storetest.New(t)
	svc := kataevidence.New(f.Store)
	const text = "Café 🙂 note. Send the budget by Friday. Send it."
	for _, tc := range []struct {
		name, column, body, quote, excerpt string
		start                              int
		noSourceID                         bool
		err                                error
	}{
		{name: "quote", body: text, quote: "Send the budget by Friday.", excerpt: "Send the budget by Friday.", start: 13},
		{name: "missing quote", body: text, quote: "Send the budget on Monday.", err: kataevidence.ErrQuoteNotFound},
		{name: "repeated quote", body: text, quote: "Send", err: kataevidence.ErrQuoteAmbiguous},
		{name: "HTML-only body", column: "body_html", body: "<p>Send the <b>budget</b> &amp; notes</p>", excerpt: "Send the budget & notes"},
		{name: "no source message ID", body: "Send the budget", noSourceID: true, err: kataevidence.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			id := f.CreateMessage(tc.name)
			if tc.noSourceID {
				_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET source_message_id=NULL WHERE id=?"), id)
				require.NoError(err)
			}
			column := cmp.Or(tc.column, "body_text")
			_, err := f.Store.DB().Exec(f.Store.Rebind("INSERT INTO message_bodies (message_id,"+column+") VALUES (?,?)"), id, tc.body)
			require.NoError(err)
			selector := kataevidence.Selector{Kind: "message", MessageID: id, Quote: tc.quote}
			if tc.quote == "" {
				selector.MaxChars = new(kataevidence.MaxChars)
			}
			prepared, err := svc.Prepare(t.Context(), []kataevidence.Selector{selector})
			if tc.err != nil {
				require.ErrorIs(err, tc.err)
				return
			}
			require.NoError(err)
			assert.Equal(t, tc.excerpt, prepared[0].Excerpt)
			assert.Equal(t, tc.start, prepared[0].Reference.Message.StartRune, "runes, not bytes, before the quote")
		})
	}
}

func TestReferencesLongerThanMaxCharsAreRejected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	svc := kataevidence.New(storetest.New(t).Store)
	for _, selectors := range [][]kataevidence.Selector{
		nil,
		make([]kataevidence.Selector, kataevidence.MaxReferences+1),
		{{Kind: "message", MessageID: 1, MaxChars: new(kataevidence.MaxChars + 1)}},
		{{Kind: "message", MessageID: 1, StartRune: new(5), EndRune: new(kataevidence.MaxChars + 6)}},
		{{Kind: "message", MessageID: 1, MaxChars: new(1), EndRune: new(1)}},
	} {
		_, err := svc.Prepare(t.Context(), selectors)
		require.ErrorIs(err, kataevidence.ErrInvalidReference)
	}
	ref := kataevidence.Reference{Version: 1, Kind: "message", ArchiveUID: "archive", MessageID: 1, SourceType: "email", SourceIdentifier: "inbox@example.com", SourceMessageID: "m1",
		Message: &kataevidence.MessageReference{BodySHA256: strings.Repeat("AB", 32), StartRune: 0, EndRune: kataevidence.MaxChars + 1}}
	_, err := kataevidence.Canonicalize(ref)
	require.ErrorIs(err, kataevidence.ErrInvalidReference)
	ref.Message.EndRune = kataevidence.MaxChars
	upper, err := kataevidence.Canonicalize(ref)
	require.NoError(err)
	assert.Equal(strings.Repeat("AB", 32), ref.Message.BodySHA256, "canonicalization must not mutate the caller")
	ref.Message.BodySHA256 = strings.Repeat("ab", 32)
	lower, err := kataevidence.Canonicalize(ref)
	require.NoError(err)
	assert.Equal(kataevidence.ID(upper), kataevidence.ID(lower))
}
