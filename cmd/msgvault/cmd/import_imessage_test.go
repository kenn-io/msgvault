package cmd

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		{"capture stores the bytes in the archive", false, true},
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

			opts, err := buildImessageOpts(slog.Default(), archive, messagesRoot, tc.noAttachments)
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
	s := testutil.NewSQLiteTestStoreAt(t, filepath.Join(t.TempDir(), "msgvault.db"))

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
