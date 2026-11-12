package cmd

import (
	"bytes"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRepairMessageCommandAuditOptionalFields(t *testing.T) {
	for _, missing := range []bool{false, true} {
		for _, jsonOut := range []bool{false, true} {
			name := "coherent"
			if missing {
				name = "missing body"
			}
			if jsonOut {
				name += "/json"
			} else {
				name += "/human"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				assert := assert.New(t)
				require := require.New(t)
				fixture := storetest.New(t)
				id := fixture.CreateMessage("gmail-audit")
				_, err := fixture.Store.DB().ExecContext(t.Context(),
					fixture.Store.Rebind(`UPDATE messages SET subject = 'stored' WHERE id = ?`), id)
				require.NoError(err)
				raw := testemail.NewMessage().From("").To("").Subject("stored").Body("body").Bytes()
				parsed, err := mime.Parse(raw)
				require.NoError(err)
				require.False(parsed.Incomplete)
				require.NoError(fixture.Store.UpsertMessageRaw(id, raw))
				if !missing {
					require.NoError(fixture.Store.UpsertMessageBody(id,
						sql.NullString{String: parsed.GetBodyText(), Valid: true}, sql.NullString{}))
				}
				command := newRepairMessageCmd(repairMessageCommandDeps{
					openWritableStore: func() (*store.Store, func(), error) {
						return nil, func() {}, errors.New("audit must not open writable store")
					},
					openReadOnlyStore: func() (*store.Store, func(), error) {
						return fixture.Store, func() {}, nil
					},
				})
				var output bytes.Buffer
				command.SetOut(&output)
				command.SetErr(&bytes.Buffer{})
				args := []string{"--audit"}
				if jsonOut {
					args = append(args, "--json")
				}
				command.SetArgs(args)

				require.NoError(command.Execute())
				if !missing {
					assert.Empty(output.String())
				} else if jsonOut {
					assert.Equal(`{"internal_id":`+jsonNumber(id)+`,"source_id":`+jsonNumber(fixture.Source.ID)+`,"source_message_id":"gmail-audit","status":"inconclusive","fields":{"attachment_hashes":"match","attachment_part_keys":"match","bcc":"match","body_html":"match","body_text":"inconclusive","cc":"match","from":"match","raw_mime":"match","rfc822_message_id":"match","subject":"match","to":"match"}}`+"\n", output.String())
				} else {
					assert.Equal("inconclusive message "+jsonNumber(id)+" (source "+jsonNumber(fixture.Source.ID)+", Gmail gmail-audit): attachment_hashes=match, attachment_part_keys=match, bcc=match, body_html=match, body_text=inconclusive, cc=match, from=match, raw_mime=match, rfc822_message_id=match, subject=match, to=match\n", output.String())
				}
			})
		}
	}
}
