package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// Each writer pauses after the real archive gate and before changing evidence.
// A competing decision must wait, retry its snapshot, and reject the old token.
func TestEmailFamilyDecisionEvidenceRacesPG(t *testing.T) {
	for _, kind := range []string{"raw", "label", "identity", "dedup_undo", "hard_delete", "new_copy"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			st := requirePostgreSQLStore(t)
			f := newAttrFixtureOn(t, st, "imap", "race@example.test")
			a := f.persist(attrMail{raw: "Message-ID: <shared@example.test>\r\n\r\n"})
			b := f.persist(attrMail{raw: "Message-ID: <shared@example.test>\r\n\r\n"})
			f.persist(attrMail{raw: "Message-ID: <replacement@example.test>\r\n\r\n", sourceMsgKey: "replacement"})
			label, err := st.EnsureLabel(f.source.ID, "flag", "Flag", "user")
			require.NoError(err)
			if kind == "dedup_undo" {
				_, err := st.MergeDuplicates(a, []int64{b}, "restore-family")
				require.NoError(err)
			}
			input := store.EmailFamilyDecisionInput{RequestKey: strings.Repeat("a", 32), Kind: "handled", MessageIDs: []int64{a}}
			preview, err := st.PreviewEmailFamilyDecisionContext(t.Context(), input)
			require.NoError(err)
			input.ExpectedFamilyRevision = preview.BeforeRevision
			reached, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			restore := st.SetAttributionAfterLockHookForTest(func([]int64) { once.Do(func() { close(reached); <-release }) })
			defer restore()
			writerDone := make(chan error, 1)
			go func() {
				var err error
				switch kind {
				case "new_copy":
					_, err = st.PersistMessageContext(context.Background(), &store.MessagePersistData{Message: &store.Message{SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "new-copy", MessageType: "email"}, RawMIME: []byte("Message-ID: <shared@example.test>\r\n\r\n")})
				case "raw":
					err = st.UpsertMessageRawWithFormatContext(context.Background(), b, []byte("Message-ID: <shared@example.test>\r\nReferences: <changed@example.test>\r\n\r\n"), "mime")
				case "label":
					err = st.AddMessageLabels(b, []int64{label})
				case "identity":
					err = st.AddAccountIdentity(f.source.ID, "alias@example.test", "manual")
				case "dedup_undo":
					_, err = st.UndoDedup("restore-family")
				case "hard_delete":
					err = st.MigrateSourceMessageID(f.source.ID, f.conv, "m-2", "replacement")
				}
				writerDone <- err
			}()
			select {
			case <-reached:
			case <-t.Context().Done():
				require.NoError(t.Context().Err())
			}
			decisionDone := make(chan error, 1)
			go func() { _, err := st.ApplyEmailFamilyDecisionContext(t.Context(), input); decisionDone <- err }()
			waitForLockWait(t, st, "archive_metadata", "decision must wait for evidence writer")
			unblock()
			require.NoError(waitResult(t, writerDone, "evidence writer"))
			require.ErrorIs(waitResult(t, decisionDone, "family decision"), store.ErrEmailFamilyChanged)
		})
	}
}
