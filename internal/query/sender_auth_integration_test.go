package query_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderAuthReadPaths(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	e := originalEngine(f)
	id := f.NewMessage().WithSubject("Sender receipt").Create(t, f.Store)
	pid := f.EnsureParticipant("sender@example.test", "Sender", "example.test")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES (?,?,'from','sender@example.test')`), id, pid)
	must.NoError(err)
	must.NoError(f.Store.UpsertMessageRaw(id, []byte("From: sender@example.test\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\nBody")))
	pins := map[int64]senderauth.TrustSet{f.Source.ID: {"mx.test"}}
	e.SetSenderAuthPins(pins)
	pins[f.Source.ID][0] = "evil.test"
	detail, err := e.GetMessage(ctx, id)
	must.NoError(err)
	must.NotNil(detail)
	must.NotNil(detail.SenderAuth)
	checks.True(detail.SenderAuth.Verified)
	summaries, err := e.GetMessageSummariesByIDs(ctx, []int64{id})
	must.NoError(err)
	must.Len(summaries, 1)
	checks.Equal(detail.SenderAuth, summaries[0].SenderAuth)
	page, err := e.ListThread(ctx, query.ThreadQuery{ID: id})
	must.NoError(err)
	must.Len(page.Messages, 1)
	checks.Equal(detail.SenderAuth, page.Messages[0].SenderAuth)
	list, err := e.ListMessages(ctx, query.MessageFilter{Pagination: query.Pagination{Limit: 10}})
	must.NoError(err)
	must.Len(list, 1)
	checks.Equal(detail.SenderAuth, list[0].SenderAuth)
	restricted := sourcescope.WithSourceIDs(ctx, []int64{f.Source.ID + 100})
	checks.False(e.ReadSenderAuth(restricted, []int64{id})[id].Verified)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET identity_is_from_me=TRUE,is_from_me=TRUE,source_is_from_me=FALSE WHERE id=?`), id)
	must.NoError(err)
	checks.True(e.ReadSenderAuth(ctx, []int64{id})[id].Verified)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET source_is_from_me=TRUE WHERE id=?`), id)
	must.NoError(err)
	checks.True(e.ReadSenderAuth(ctx, []int64{id})[id].Verified, "legacy ownership flags cannot suppress receipt evidence")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET source_is_from_me=FALSE WHERE id=?`), id)
	must.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE message_raw SET encryption_version=1 WHERE message_id=?`), id)
	must.NoError(err)
	checks.Equal(senderauth.Basis("no_raw"), e.ReadSenderAuth(ctx, []int64{id})[id].Basis)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE message_raw SET encryption_version=0 WHERE message_id=?`), id)
	must.NoError(err)
	label, err := f.Store.EnsureLabel(f.Source.ID, "SENT", "Sent", "system")
	must.NoError(err)
	must.NoError(f.Store.AddMessageLabels(id, []int64{label}))
	checks.Equal(senderauth.Basis("owner_sent"), e.ReadSenderAuth(ctx, []int64{id})[id].Basis)
}

func TestSenderAuthUsesPersistedIMAPSentOriginAfterLabelChanges(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("imap", "imap://sent-origin@example.test:993")
	requirements.NoError(err)
	conversationID, err := f.Store.EnsureConversation(source.ID, "sent-origin", "Sent origin")
	requirements.NoError(err)
	messageID := storetest.NewMessage(source.ID, conversationID).WithSourceMessageID("imap-sent").Create(t, f.Store)
	requirements.NoError(f.Store.SetMessageMetadata(messageID, sql.NullString{
		String: `{"imap_content_origin":"sent"}`,
		Valid:  true,
	}))
	labels, err := f.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"SENT": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	requirements.NoError(err)
	requirements.NoError(f.Store.AddMessageLabels(messageID, []int64{labels["SENT"]}))
	requirements.NoError(f.Store.RemoveMessageLabels(messageID, []int64{labels["SENT"]}))

	verdict := originalEngine(f).ReadSenderAuth(t.Context(), []int64{messageID})[messageID]
	requirements.NotNil(verdict)
	requirements.Equal(senderauth.Verdict{Kind: "none", Basis: "owner_sent"}, *verdict)
}
