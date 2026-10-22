package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

type rawMIMEPolicyImport func(*testing.T, *store.Store, string, string, bool) (int64, bool, error)

func TestRawMIMEAttachmentFailureRespectsNotificationChoice(t *testing.T) {
	raw := testemail.NewMessage().
		From("sender@example.net").
		To("owner@example.net").
		Header("Message-ID", "<synthetic-attachment@example.net>").
		Body("synthetic body").
		WithAttachment("synthetic.txt", "text/plain", []byte("synthetic attachment")).
		Bytes()

	cases := []struct {
		name       string
		sourceType string
		makeInput  func(*testing.T, string, []byte) string
		importRun  rawMIMEPolicyImport
	}{
		{
			name: "eml", sourceType: "eml",
			makeInput: func(t *testing.T, tmp string, raw []byte) string {
				t.Helper()
				require := Require.New(t)
				root := filepath.Join(tmp, "Synthetic.mailbox")
				require.NoError(os.MkdirAll(root, 0o700))
				require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), raw, 0o600))
				return root
			},
			importRun: func(t *testing.T, st *store.Store, input, attachments string, notify bool) (int64, bool, error) {
				t.Helper()
				summary, err := ImportEMLDir(t.Context(), st, input, EMLImportOptions{Identifier: "owner@example.net", NoResume: true, NotifyImports: notify, AttachmentsDir: attachments})
				if summary == nil {
					return 0, false, err
				}
				return summary.MessagesAdded, summary.HardErrors, err
			},
		},
		{
			name: "emlx", sourceType: "apple-mail",
			makeInput: func(t *testing.T, tmp string, raw []byte) string {
				t.Helper()
				require := Require.New(t)
				root := filepath.Join(tmp, "Mail")
				mailbox := filepath.Join(root, "Mailboxes", "Synthetic.mbox", "Messages")
				require.NoError(os.MkdirAll(mailbox, 0o700))
				mkEmlx(t, mailbox, "1.emlx", raw)
				return root
			},
			importRun: func(t *testing.T, st *store.Store, input, attachments string, notify bool) (int64, bool, error) {
				t.Helper()
				summary, err := ImportEmlxDir(t.Context(), st, input, EmlxImportOptions{Identifier: "owner@example.net", NoResume: true, NotifyImports: notify, AttachmentsDir: attachments})
				if summary == nil {
					return 0, false, err
				}
				return summary.MessagesAdded, summary.HardErrors, err
			},
		},
		{
			name: "pst", sourceType: "pst",
			makeInput: func(t *testing.T, tmp string, _ []byte) string {
				t.Helper()
				require := Require.New(t)
				path := filepath.Join(tmp, "synthetic-media.pst")
				require.NoError(os.WriteFile(path, syntheticPSTMediaFixture, 0o600))
				return path
			},
			importRun: func(t *testing.T, st *store.Store, input, attachments string, notify bool) (int64, bool, error) {
				t.Helper()
				summary, err := ImportPst(t.Context(), st, input, PstImportOptions{Identifier: "owner@example.net", NoResume: true, NotifyImports: notify, AttachmentsDir: attachments})
				if summary == nil {
					return 0, false, err
				}
				return summary.MessagesAdded, summary.HardErrors, err
			},
		},
	}

	for _, tc := range cases {
		for _, notify := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/silent", true: "/notifying"}[notify], func(t *testing.T) {
				assert := Assert.New(t)
				require := Require.New(t)
				st, tmp := testutil.NewTestStore(t), t.TempDir()
				_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
					Enabled: true, Principal: "owner:synthetic-raw-mime-attachment",
					ImportNotifications: true,
					Capabilities: []store.MCPEventCapability{{
						Family: "msgvault.import_message_archived", SourceType: tc.sourceType, Kinds: []string{"message"},
					}},
				})
				require.NoError(err)

				input := tc.makeInput(t, tmp, raw)
				attachments := filepath.Join(tmp, "attachments")
				require.NoError(os.WriteFile(attachments, []byte("synthetic not a directory"), 0o600))
				added, hardErrors, err := tc.importRun(t, st, input, attachments, notify)
				require.NoError(err)

				var messages, attachmentRows, events, decisions int
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&messages))
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM attachments`).Scan(&attachmentRows))
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM import_message_decisions`).Scan(&decisions))
				assert.Zero(events)
				if notify {
					assert.Zero(messages, "a selected attachment must be ready before a notifying message commits")
					assert.Zero(attachmentRows)
					assert.Zero(decisions, "failed readiness must not consume the first-import decision")
					assert.Zero(added)
					assert.True(hardErrors)
					return
				}

				assert.Equal(1, messages, "silent import should keep its message when best-effort attachment storage fails")
				assert.Zero(attachmentRows)
				assert.Equal(1, decisions, "silent imports keep the durable first-import decision")
				assert.Zero(hardErrors)
				assert.Equal(int64(1), added)
			})
		}
	}
}
