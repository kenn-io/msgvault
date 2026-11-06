package rederive

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailUnusableRawPreservesDerivedText(t *testing.T) {
	for _, mode := range []string{"manual", "stale"} {
		for _, rawState := range []string{"empty-zlib", "empty-uncompressed", "invalid-zlib", "invalid-mime", "absent"} {
			t.Run(mode+"/"+rawState, func(t *testing.T) {
				r, a := require.New(t), assert.New(t)
				st, src, id := archiveEmailSource(t, "gmail", "retainedtoken")
				wantBody := sql.NullString{String: "retainedtoken", Valid: true}
				wantHTML := sql.NullString{String: "<p>retained HTML</p>", Valid: true}
				r.NoError(st.UpsertMessageBody(id, wantBody, wantHTML))
				_, err := st.DB().Exec(st.Rebind("UPDATE messages SET snippet = ? WHERE id = ?"), wantBody.String, id)
				r.NoError(err)
				switch rawState {
				case "empty-zlib":
					r.NoError(st.UpsertMessageRaw(id, []byte{}))
				case "empty-uncompressed", "invalid-zlib":
					data, compression := []byte{}, "none"
					if rawState == "invalid-zlib" {
						data, compression = []byte("invalid zlib"), "zlib"
					}
					_, err = st.DB().Exec(st.Rebind("UPDATE message_raw SET raw_data = ?, compression = ? WHERE message_id = ?"), data, compression, id)
					r.NoError(err)
				case "invalid-mime":
					r.NoError(st.UpsertMessageRaw(id, []byte("invalid MIME")))
				case "absent":
					_, err = st.DB().Exec(st.Rebind("DELETE FROM message_raw WHERE message_id = ?"), id)
					r.NoError(err)
				}
				var rawBefore []byte
				var compressionBefore sql.NullString
				if rawState != "absent" {
					r.NoError(st.DB().QueryRow(st.Rebind("SELECT raw_data, compression FROM message_raw WHERE message_id = ?"), id).Scan(&rawBefore, &compressionBefore))
				}
				var sum *Summary
				if mode == "manual" {
					sum, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
				} else {
					var ran bool
					sum, ran, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
					a.True(ran)
				}
				r.NoError(err)
				r.NotNil(sum)
				var body, html, snippet sql.NullString
				r.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text, body_html FROM message_bodies WHERE message_id = ?"), id).Scan(&body, &html))
				r.NoError(st.DB().QueryRow(st.Rebind("SELECT snippet FROM messages WHERE id = ?"), id).Scan(&snippet))
				a.Equal(wantBody, body)
				a.Equal(wantHTML, html)
				a.Equal(wantBody, snippet)
				matches, _, err := st.SearchMessages("retainedtoken", 0, 10)
				r.NoError(err)
				a.Len(matches, 1, "the last searchable content must survive")
				a.Zero(sum.BodiesRewritten)
				a.Zero(sum.Errors)
				a.Zero(derivedDataRevision(t, st))
				if rawState == "absent" {
					a.Zero(sum.MessagesScanned, "only retained MIME is scanned")
					a.Zero(sum.Undecodable)
					var count int
					r.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM message_raw WHERE message_id = ?"), id).Scan(&count))
					a.Zero(count)
				} else {
					a.Equal(int64(1), sum.MessagesScanned)
					a.Equal(int64(1), sum.Undecodable, "an unusable source must be reported")
					var rawAfter []byte
					var compressionAfter sql.NullString
					r.NoError(st.DB().QueryRow(st.Rebind("SELECT raw_data, compression FROM message_raw WHERE message_id = ?"), id).Scan(&rawAfter, &compressionAfter))
					a.Equal(rawBefore, rawAfter)
					a.Equal(compressionBefore, compressionAfter)
				}
				applied, err := st.IsMigrationApplied(LedgerKey(src.SourceType, src.Identifier, emailVersion))
				r.NoError(err)
				a.True(applied, "the ledger records a completed scan, including reported undecodable rows")
			})
		}
	}
}

func TestEmailUsableRawCanRemoveObsoleteText(t *testing.T) {
	for _, body := range []string{"", "replacementtoken"} {
		t.Run("body="+body, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			st, src, id := archiveEmailSource(t, "gmail", "retainedtoken")
			raw := []byte("From: sender@example.net\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + body)
			r.NoError(st.UpsertMessageRaw(id, raw))
			_, err := st.DB().Exec(st.Rebind("UPDATE messages SET snippet = ? WHERE id = ?"), "retainedtoken", id)
			r.NoError(err)
			sum, err := Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
			r.NoError(err)
			a.Zero(sum.Undecodable)
			a.Zero(sum.Errors)
			a.Equal(int64(1), sum.BodiesRewritten)
			var gotBody, html, snippet sql.NullString
			r.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text, body_html FROM message_bodies WHERE message_id = ?"), id).Scan(&gotBody, &html))
			r.NoError(st.DB().QueryRow(st.Rebind("SELECT snippet FROM messages WHERE id = ?"), id).Scan(&snippet))
			a.Equal(sql.NullString{String: body, Valid: body != ""}, gotBody)
			a.Equal(sql.NullString{}, html, "valid plain MIME supersedes obsolete HTML")
			a.Equal(gotBody, snippet)
			matches, _, err := st.SearchMessages("retainedtoken", 0, 10)
			r.NoError(err)
			a.Empty(matches, "obsolete body must leave search")
			gotRaw, err := st.GetMessageRaw(id)
			r.NoError(err)
			a.Equal(raw, gotRaw)
			a.Positive(derivedDataRevision(t, st))
		})
	}
}
