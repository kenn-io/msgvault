package sync

import (
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestSenderRelationshipsFromProviderSync(t *testing.T) {
	for _, provider := range []string{"gmail", "imap"} {
		for _, sent := range []bool{false, true} {
			name := provider + "/draft"
			if sent {
				name = provider + "/sent"
			}
			t.Run(name, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				env := newTestEnv(t)
				raw := testemail.NewMessage().From(testEmail).To("target@example.com").
					Header("Message-ID", "<relationship@example.com>").
					Subject("Synthetic relationship").Body("Hello").Bytes()
				if provider == "gmail" {
					label := "DRAFT"
					if sent {
						label = "SENT"
					}
					env.Mock.Profile.MessagesTotal = 1
					env.Mock.MessagePages = [][]string{{"native-message"}}
					env.Mock.AddMessage("native-message", raw, []string{label})
					runFullSync(t, env)
				} else {
					mailbox, role := "Brouillons", imapv2.MailboxAttrDrafts
					if sent {
						// A native IMAP mailbox name can share an import prefix. Its
						// mailbox|UID ID must remain distinct from a raw EML hash ID.
						mailbox, role = "eml-sent", imapv2.MailboxAttrSent
					}
					addr, user := testutil.StartIMAPMemServerWithSpecialUse(t,
						map[string]int{mailbox: 0, "INBOX": 0},
						map[string][]imapv2.MailboxAttr{mailbox: {role}})
					testutil.AppendIMAPRawMessage(t, user, mailbox, raw)
					opts := DefaultOptions()
					opts.SourceType = sourceTypeIMAP
					client := newSyncTestIMAPClient(t, addr)
					env.Syncer = New(client, env.Store, opts)
					runFullSync(t, env)
					requirements.NoError(client.Close())
					if sent {
						var sourceMessageID string
						requirements.NoError(env.Store.DB().QueryRow(`SELECT source_message_id FROM messages`).Scan(&sourceMessageID))
						assertions.Equal("eml-sent|1", sourceMessageID)
						got, err := env.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
						requirements.NoError(err)
						assertions.Equal(store.SenderRelationshipEvidence{Relationship: "known", SeenInArchive: true}, got["target@example.com"])

						// Sent origin must remain usable after the message leaves Sent.
						testutil.AppendIMAPRawMessage(t, user, "INBOX", raw)
						testutil.ExpungeIMAPMessage(t, addr, mailbox, imapv2.UID(1))
						client = newSyncTestIMAPClient(t, addr)
						env.Syncer = New(client, env.Store, opts)
						runFullSync(t, env)
						requirements.NoError(client.Close())
						var origin string
						requirements.NoError(env.Store.DB().QueryRow(`SELECT metadata ->> 'imap_content_origin' FROM messages`).Scan(&origin))
						assertions.Equal("sent", origin)
						got, err = env.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
						requirements.NoError(err)
						assertions.Equal(store.SenderRelationshipEvidence{Relationship: "known", SeenInArchive: true}, got["target@example.com"])
					}
				}
				var sourceOwnership bool
				requirements.NoError(env.Store.DB().QueryRow(`SELECT source_is_from_me FROM messages`).Scan(&sourceOwnership))
				assertions.False(sourceOwnership, "email sync retains provider ownership separately from native chat ownership")
				got, err := env.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
				requirements.NoError(err)
				want := store.SenderRelationshipEvidence{Relationship: "none"}
				if sent {
					want = store.SenderRelationshipEvidence{Relationship: "known", SeenInArchive: true}
				}
				assertions.Equal(want, got["target@example.com"])
			})
		}
	}
}
