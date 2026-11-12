package sync

import (
	"database/sql"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestAuditGmailMessagesOptionalFields(t *testing.T) {
	for _, test := range []struct {
		name       string
		query      string
		raw        []byte
		emptyHTML  bool
		wantStatus AuditState // Empty means the coherent row must be omitted.
		wantFields map[string]AuditState
	}{
		{name: "coherent"},
		{
			name: "missing body row", query: `DELETE FROM message_bodies WHERE message_id = ?`,
			wantStatus: AuditInconclusive, wantFields: map[string]AuditState{AuditFieldBodyText: AuditInconclusive},
		},
		{
			name: "null body text", query: `UPDATE message_bodies SET body_text = NULL WHERE message_id = ?`,
			wantStatus: AuditInconclusive, wantFields: map[string]AuditState{AuditFieldBodyText: AuditInconclusive},
		},
		{
			name: "missing HTML", raw: testemail.NewMessage().Subject("stored").
				Header("Message-ID", "<gmail-a@example.com>").ContentType("text/html").Body("<p>stored body</p>").Bytes(),
			wantStatus: AuditInconclusive, wantFields: map[string]AuditState{AuditFieldBodyHTML: AuditInconclusive},
		},
		{
			name: "missing message ID", query: `UPDATE messages SET rfc822_message_id = NULL WHERE id = ?`,
			wantStatus: AuditInconclusive, wantFields: map[string]AuditState{AuditFieldRFC822MessageID: AuditInconclusive},
		},
		{
			name: "multiple inconclusive fields", query: `UPDATE messages SET subject = NULL, rfc822_message_id = NULL WHERE id = ?`,
			wantStatus: AuditInconclusive, wantFields: map[string]AuditState{
				AuditFieldSubject: AuditInconclusive, AuditFieldRFC822MessageID: AuditInconclusive,
			},
		},
		{
			name: "mismatch takes precedence", query: `UPDATE messages SET subject = 'different', rfc822_message_id = NULL WHERE id = ?`,
			wantStatus: AuditMismatch, wantFields: map[string]AuditState{
				AuditFieldSubject: AuditMismatch, AuditFieldRFC822MessageID: AuditInconclusive,
			},
		},
		{
			name: "determined mismatch", query: `UPDATE messages SET subject = 'different' WHERE id = ?`, emptyHTML: true,
			wantStatus: AuditMismatch, wantFields: map[string]AuditState{AuditFieldSubject: AuditMismatch},
		},
		{
			name: "stored empty is authoritative", query: `UPDATE message_bodies SET body_text = '' WHERE message_id = ?`,
			wantStatus: AuditMismatch, wantFields: map[string]AuditState{AuditFieldBodyText: AuditMismatch},
		},
		{
			name: "legitimate absent headers and bodies", query: `UPDATE messages SET subject = NULL, rfc822_message_id = NULL WHERE id = ?`,
			raw: []byte("From: sender@example.com\r\nTo: recipient@example.com\r\nContent-Type: text/plain\r\n\r\n"),
		},
		{
			name: "decode error", query: `UPDATE message_raw SET raw_data = 'not-zlib', compression = 'zlib' WHERE message_id = ?`,
			wantStatus: AuditInconclusive, wantFields: map[string]AuditState{AuditFieldRawMIME: AuditInconclusive},
		},
		{
			name: "parse error", raw: []byte("not MIME\x00"),
			wantStatus: AuditInconclusive, wantFields: map[string]AuditState{AuditFieldRawMIME: AuditInconclusive},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", "audit@example.com")
			require.NoError(err)
			id := seedRepairRow(t, st, source.ID, "gmail-a", "thread-a", "stored")
			if test.query != "" {
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(test.query), id)
				require.NoError(err)
			}
			if test.raw != nil {
				require.NoError(st.UpsertMessageRaw(id, test.raw))
			}
			if test.emptyHTML {
				_, err = st.DB().ExecContext(t.Context(),
					st.Rebind(`UPDATE message_bodies SET body_html = '' WHERE message_id = ?`), id)
				require.NoError(err)
			}
			if test.name == "missing HTML" {
				parsed, parseErr := mime.Parse(test.raw)
				require.NoError(parseErr)
				require.NoError(st.UpsertMessageBody(id,
					sql.NullString{String: parsed.GetBodyText(), Valid: true}, sql.NullString{}))
			}
			if test.name == "legitimate absent headers and bodies" {
				parsed, parseErr := mime.Parse(test.raw)
				require.NoError(parseErr)
				require.False(parsed.Incomplete)
				require.Empty(parsed.GetBodyText())
				require.Empty(parsed.BodyHTML)
				require.Empty(parsed.Subject)
				require.Empty(parsed.MessageID)
				require.NoError(st.UpsertMessageBody(id, sql.NullString{}, sql.NullString{}))
			}

			results := collectAudit(t, New(nil, st, nil), source.ID)
			if test.wantStatus == "" {
				assert.Empty(results)
				return
			}
			require.Len(results, 1)
			result := results[0]
			assert.Equal(test.wantStatus, result.Status)
			if test.wantFields[AuditFieldRawMIME] == AuditInconclusive {
				assert.Equal(test.wantFields, result.Fields)
				assert.NotEmpty(result.Error)
				return
			}
			wantFields := map[string]AuditState{
				AuditFieldRawMIME: AuditMatch, AuditFieldRFC822MessageID: AuditMatch,
				AuditFieldSubject: AuditMatch, AuditFieldBodyText: AuditMatch, AuditFieldBodyHTML: AuditMatch,
				AuditFieldFrom: AuditMatch, AuditFieldTo: AuditMatch, AuditFieldCC: AuditMatch, AuditFieldBCC: AuditMatch,
				AuditFieldAttachmentHashes: AuditMatch, AuditFieldAttachmentPartKeys: AuditMatch,
			}
			maps.Copy(wantFields, test.wantFields)
			assert.Equal(wantFields, result.Fields)
			assert.Empty(result.Error, "missing fields must not acquire a speculative absence reason")
		})
	}
}
