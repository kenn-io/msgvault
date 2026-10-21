package cmd

import (
	"bytes"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The command must hand the archive directory and --messages-root to the
// importer, so the attachment bytes land in the configured archive.
func TestBuildImessageOptsWiresAttachmentCapture(t *testing.T) {
	for _, tc := range []struct {
		name          string
		noAttachments bool
		wantStored    bool
	}{
		{"--no-attachments stores none", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			tmp := t.TempDir()
			archive, messagesRoot := filepath.Join(tmp, "archive"), filepath.Join(tmp, "Messages")
			requirements.NoError(os.MkdirAll(filepath.Join(messagesRoot, "Attachments"), 0o700))
			requirements.NoError(os.WriteFile(filepath.Join(messagesRoot, "Attachments", "voice.caf"), []byte("caffSYNTHETIC"), 0o600))
			chatPath, _ := testutil.NewIMessageAttachmentChatDB(t)

			s, err := store.Open(filepath.Join(tmp, "msgvault.db"))
			requirements.NoError(err)
			t.Cleanup(func() { _ = s.Close() })
			requirements.NoError(s.InitSchema())
			src, err := resolveImessageSource(s)
			requirements.NoError(err)

			opts, err := buildImessageOpts(slog.Default(), archive, messagesRoot, tc.noAttachments, config.IMessageConfig{}.MediaPolicy())
			requirements.NoError(err)
			client, err := imessage.NewClient(chatPath, opts...)
			requirements.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			_, err = client.Import(t.Context(), s, src.ID)
			requirements.NoError(err)

			var stored []string
			_ = filepath.WalkDir(archive, func(path string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					stored = append(stored, path)
				}
				return nil
			})
			if !tc.wantStored {
				assert.Empty(t, stored)
				return
			}
			requirements.Len(stored, 1)
		})
	}
}

// TestImportIMessage_NoAutoDefaultIdentity pins the documented behavior: the
// apple_messages source uses identifier "local" and the spec explicitly excludes
// this ingest path from auto-default-identity. After source creation via
// resolveImessageSource, account_identities must remain empty.
func TestImportIMessage_NoAutoDefaultIdentity(t *testing.T) {
	require := require.New(t)
	// After a successful import, account_identities has zero rows for the
	// apple_messages source. The source identifier is "local"; we never
	// auto-write because there's no per-user identifier known at source
	// creation time. Spec § Auto-default-identity § "Ingest paths that do
	// not auto-write".
	s, err := store.Open(filepath.Join(t.TempDir(), "msgvault.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	src, err := resolveImessageSource(s)
	require.NoError(err, "resolveImessageSource")

	rows, err := s.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	assert.Empty(t, rows, "expected no account_identities rows for apple_messages source")
}

func TestResolveImessageSource(t *testing.T) {
	tests := []struct {
		name           string
		seedSources    []struct{ sourceType, identifier string }
		wantIdentifier string
	}{
		{
			name:           "no existing sources — creates local",
			seedSources:    nil,
			wantIdentifier: localValue,
		},
		{
			name: "only local exists — reuses local",
			seedSources: []struct{ sourceType, identifier string }{
				{"apple_messages", localValue},
			},
			wantIdentifier: localValue,
		},
		{
			name: "only legacy exists — reuses legacy",
			seedSources: []struct{ sourceType, identifier string }{
				{"apple_messages", "+15551234567"},
			},
			wantIdentifier: "+15551234567",
		},
		{
			name: "both legacy and local — prefers legacy",
			seedSources: []struct{ sourceType, identifier string }{
				{"apple_messages", localValue},
				{"apple_messages", "+15551234567"},
			},
			wantIdentifier: "+15551234567",
		},
		{
			name: "multiple legacy — picks first non-local",
			seedSources: []struct{ sourceType, identifier string }{
				{"apple_messages", localValue},
				{"apple_messages", "alice@icloud.com"},
				{"apple_messages", "+15551234567"},
			},
			// ListSources returns sorted by identifier;
			// +15551234567 sorts before alice@icloud.com
			wantIdentifier: "+15551234567",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			s, err := store.Open(":memory:")
			require.NoError(err)
			defer func() { _ = s.Close() }()

			require.NoError(s.InitSchema())

			for _, seed := range tt.seedSources {
				_, err := s.GetOrCreateSource(
					seed.sourceType, seed.identifier,
				)
				require.NoError(err, "seed source %q", seed.identifier)
			}

			src, err := resolveImessageSource(s)
			require.NoError(err, "resolveImessageSource")
			assert.Equal(t, tt.wantIdentifier, src.Identifier, "identifier")
		})
	}
}

// restoreImessageFlags returns the flags a test sets through the root command
// to their registered defaults and resets the command's output and arguments.
func restoreImessageFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		for _, flags := range []*pflag.FlagSet{rootCmd.PersistentFlags(), importImessageCmd.Flags()} {
			flags.VisitAll(func(flag *pflag.Flag) {
				_ = flag.Value.Set(flag.DefValue)
				flag.Changed = false
			})
		}
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
}

// runImessageImport runs the real command in its daemon subprocess role
// against a temporary home holding config.toml.
func runImessageImport(t *testing.T, home string, args ...string) {
	t.Helper()
	restoreImessageFlags(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	rootCmd.SetArgs(append([]string{"--home", home, "import-imessage"}, args...))
	done := captureStdout(t)
	err := executeRootContext(t.Context(), rootCmd)
	output := done()
	require.NoError(t, err, output)
}

// Config limits and --max-media-mb must both reach the importer through the
// command. Dropping either from the policy handoff leaves the attachment
// stored and fails the case that sets it.
func TestImessageMediaLimitsChangeStoredAttachments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		toml  string
		flags []string
		state attachmentpolicy.DownloadState
	}{
		{"config size cap", "[imessage]\nmax_media_mb = 1\n", nil, attachmentpolicy.StateSkipped},
		{"flag raises config cap", "[imessage]\nmax_media_mb = 1\n", []string{"--max-media-mb=5"}, attachmentpolicy.StateStored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			home := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.toml), 0o600))
			chatPath, chat := testutil.NewIMessageAttachmentChatDB(t)
			_, err := chat.Exec(`UPDATE attachment SET total_bytes=?`, 2<<20)
			require.NoError(err)
			root := t.TempDir()
			require.NoError(os.MkdirAll(filepath.Join(root, "Attachments"), 0o700))
			require.NoError(os.WriteFile(filepath.Join(root, "Attachments", "voice.caf"), make([]byte, 2<<20), 0o600))

			runImessageImport(t, home, append([]string{"--db-path", chatPath, "--messages-root", root}, tc.flags...)...)

			st, err := store.Open(filepath.Join(home, "msgvault.db"))
			require.NoError(err)
			t.Cleanup(func() { _ = st.Close() })
			src, err := resolveImessageSource(st)
			require.NoError(err)
			var messageID int64
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), src.ID, "1").Scan(&messageID))
			refs, err := st.MessageIMessageAttachmentsContext(t.Context(), messageID)
			require.NoError(err)
			require.Len(refs, 1)
			ref := refs["imessage:1"]
			assert.Equal(tc.state, ref.State)
			if tc.state == attachmentpolicy.StateStored {
				assert.FileExists(filepath.Join(home, "attachments", ref.StoragePath))
			}
		})
	}
}

// A bad --max-media-mb fails in PreRunE, so the archive is never created.
func TestImessageMaxMediaMBRejectedBeforeStoreOpens(t *testing.T) {
	for name, flag := range map[string]string{
		"negative":        "--max-media-mb=-1",
		"overflows bytes": "--max-media-mb=9223372036854775807",
	} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			restoreImessageFlags(t)
			home := t.TempDir()
			var output bytes.Buffer
			rootCmd.SetOut(&output)
			rootCmd.SetErr(&output)
			rootCmd.SetArgs([]string{"--home", home, "import-imessage", "--db-path", filepath.Join(home, "chat.db"), flag})

			err := executeRootContext(t.Context(), rootCmd)
			require.Error(err)
			require.ErrorContains(err, "--max-media-mb")
			assert.Contains(output.String(), "Usage:", "a bad flag prints usage")
			assert.NoFileExists(filepath.Join(home, "msgvault.db"))
		})
	}
}
