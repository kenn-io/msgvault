package imap

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxIMAPArchiveFolderFilter(t *testing.T) {
	for _, configured := range []bool{false, true} {
		mode := "special use"
		if configured {
			mode = "configured"
		}
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name                      string
				folders, include, exclude []string
				wantError                 bool
			}{
				{name: "unrestricted"},
				{name: "config includes archive", folders: []string{"INBOX", "aRcHiVe"}},
				{name: "config omits archive", folders: []string{"INBOX"}, wantError: true},
				{name: "run include allows archive", include: []string{"aRcHiVe"}},
				{name: "run include omits archive", include: []string{"INBOX"}, wantError: true},
				{name: "run include overrides config denial", folders: []string{"INBOX"}, include: []string{"Archive"}},
				{name: "run include overrides config allowance", folders: []string{"Archive"}, include: []string{"INBOX"}, wantError: true},
				{name: "empty run include uses config", folders: []string{"INBOX"}, include: []string{}, wantError: true},
				{name: "empty name is an active include", include: []string{""}, wantError: true},
				{name: "duplicate reordered include", include: []string{"Other", "Archive", "INBOX", "Archive"}},
				{name: "exclude archive", exclude: []string{"Archive"}, wantError: true},
				{name: "exclude archive case insensitive", exclude: []string{"aRcHiVe"}, wantError: true},
				{name: "exclude wins over config", folders: []string{"Archive"}, exclude: []string{"Archive"}, wantError: true},
				{name: "exclude wins over run include", include: []string{"Archive"}, exclude: []string{"Archive"}, wantError: true},
				{name: "exclude unrelated folder", exclude: []string{"Other"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := newInboxNativeFixture(t, "special")
					client := f.provider.client
					if configured {
						client.config.ArchiveMailbox = "Archive"
					}
					client.config.Folders = tc.folders
					WithFolderFilter(tc.include, tc.exclude)(client)

					folder, err := f.provider.archiveFolder(t.Context())
					if tc.wantError {
						require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
						assert.Empty(t, folder)
					} else {
						require.NoError(t, err)
						assert.Equal(t, f.destination, folder)
					}
					assert.Zero(t, f.moves.Load())
					assert.Zero(t, f.copies.Load())
					assert.Zero(t, f.expunges.Load())
					assert.Zero(t, f.creates.Load())
				})
			}
		})
	}
}

// Explicit exclusion must win regardless of casing, other filter entries,
// include-list overrides, or how the archive destination was discovered.
func FuzzInboxIMAPArchiveExcludedFolder(f *testing.F) {
	f.Add(uint64(0), []byte{}, false)
	f.Add(^uint64(0), []byte("other"), true)
	f.Fuzz(func(t *testing.T, mask uint64, other []byte, configured bool) {
		fixture := newInboxNativeFixture(t, "special")
		client := fixture.provider.client
		if configured {
			client.config.ArchiveMailbox = "Archive"
		}
		var excluded strings.Builder
		for i, r := range "Archive" {
			letter := string(r)
			if mask&(1<<i) != 0 {
				letter = strings.ToUpper(letter)
			} else {
				letter = strings.ToLower(letter)
			}
			excluded.WriteString(letter)
		}
		// Materialize a bounded mailbox name from arbitrary bytes. Its prefix
		// keeps it distinct from the archive, including when other is empty.
		companion := "Other/" + hex.EncodeToString(other[:min(len(other), 32)])
		if mask&(1<<8) != 0 {
			client.config.Folders = []string{"INBOX", "Archive", companion}
		}
		var include []string
		if mask&(1<<9) != 0 {
			include = []string{companion, "Archive"}
		}
		WithFolderFilter(include, []string{companion, excluded.String()})(client)

		folder, err := fixture.provider.archiveFolder(t.Context())
		require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
		assert.Empty(t, folder)
	})
}

func TestInboxIMAPArchiveFilterDoesNotResolveAmbiguity(t *testing.T) {
	f := newInboxNativeFixture(t, "special ambiguous")
	WithFolderFilter([]string{"Archive"}, []string{"Other"})(f.provider.client)

	folder, err := f.provider.archiveFolder(t.Context())
	require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
	assert.Empty(t, folder)
	assert.Zero(t, f.moves.Load())
	assert.Zero(t, f.copies.Load())
	assert.Zero(t, f.expunges.Load())
	assert.Zero(t, f.creates.Load())
}

func TestInboxIMAPArchiveRechecksFolderFilterAtDispatch(t *testing.T) {
	for _, configured := range []bool{false, true} {
		mode := "special use"
		if configured {
			mode = "configured"
		}
		t.Run(mode, func(t *testing.T) {
			for _, change := range []string{"config include", "run include", "run exclude", "still allowed"} {
				t.Run(change, func(t *testing.T) {
					f := newInboxNativeFixture(t, "special")
					client := f.provider.client
					if configured {
						client.config.ArchiveMailbox = "Archive"
					}
					request := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &f.target, DryRun: true}
					before, err := f.provider.Observe(t.Context(), request)
					require.NoError(t, err)
					projected, err := f.provider.Preview(t.Context(), request, before)
					require.NoError(t, err)
					require.Equal(t, []inboxcontrol.Folder{f.destination}, projected.Folders)
					request.ResolvedFolder = &projected.Folders[0]
					request.DryRun = false

					switch change {
					case "config include":
						client.config.Folders = []string{"INBOX"}
					case "run include":
						WithFolderFilter([]string{"INBOX"}, nil)(client)
					case "run exclude":
						WithFolderFilter(nil, []string{"Archive"})(client)
					}
					dispatch, err := f.provider.Dispatch(t.Context(), request, before)
					if change == "still allowed" {
						require.NoError(t, err)
						require.NotNil(t, dispatch.Target)
						want := f.target
						want.Mailbox, want.UIDValidity, want.UID = "Archive", f.destination.UIDValidity, 2
						assert.Equal(t, want, *dispatch.Target)
						assert.Equal(t, int64(1), f.moves.Load())
					} else {
						require.ErrorIs(t, err, inboxcontrol.ErrNoWrite)
						assert.Nil(t, dispatch.Target)
						assert.Zero(t, f.moves.Load())
					}
					assert.Zero(t, f.copies.Load())
					assert.Zero(t, f.expunges.Load())
					assert.Zero(t, f.creates.Load())
				})
			}
		})
	}
}

func FuzzInboxIMAPArchiveConfiguredFolderIdentity(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("archive"))
	f.Fuzz(func(t *testing.T, suffix []byte) {
		fixture := newInboxNativeFixture(t, "native")
		client := fixture.provider.client
		// Exercise a real nested mailbox with spaces and Unicode. Cap only
		// its materialization so arbitrary fuzz inputs stay cheap to serve.
		name := "Other/保存 Archive " + hex.EncodeToString(suffix[:min(len(suffix), 32)])
		var epoch uint32
		require.NoError(t, client.withConn(t.Context(), func(conn *imapclient.Client) error {
			if err := conn.Create(name, nil).Wait(); err != nil {
				return fmt.Errorf("prepare configured archive in fixture: %w", err)
			}
			status, err := conn.Status(name, &imapapi.StatusOptions{UIDValidity: true}).Wait()
			if err != nil {
				return fmt.Errorf("prepare configured archive in fixture: %w", err)
			}
			epoch = status.UIDValidity
			return nil
		}))
		require.NotZero(t, epoch)
		client.config.ArchiveMailbox = name
		client.config.Folders = []string{"INBOX", strings.ToUpper(name), strings.ToUpper(name)}

		folder, err := fixture.provider.archiveFolder(t.Context())
		require.NoError(t, err)
		assert.Equal(t, inboxcontrol.Folder{ID: name, Name: name, UIDValidity: epoch}, folder)
	})
}
