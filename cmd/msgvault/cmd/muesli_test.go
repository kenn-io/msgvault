package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveMuesliSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	previous := cfg
	t.Cleanup(func() { cfg = previous })

	cfg = &config.Config{}
	_, err := resolveMuesliSources(nil)
	require.Error(err)
	assert.Contains(err.Error(), "[[muesli]]")

	cfg = &config.Config{Muesli: []config.MuesliSource{
		{Identifier: "mac", AccountEmail: "you@example.com"},
		{Identifier: "studio", AccountEmail: "you@example.com"},
	}}
	all, err := resolveMuesliSources(nil)
	require.NoError(err)
	assert.Len(all, 2)

	one, err := resolveMuesliSources([]string{"STUDIO"})
	require.NoError(err)
	require.Len(one, 1)
	assert.Equal("studio", one[0].Identifier)

	_, err = resolveMuesliSources([]string{"laptop"})
	require.Error(err)
	assert.Contains(err.Error(), "configured: mac, studio")
}

func TestProbeMuesliDatabaseRejectsForeignFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("not sqlite"), 0o600))

	err := probeMuesliDatabase(context.Background(), path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "db_path")
}

func TestRunConfiguredMuesliSyncRefusesUnregisteredSource(t *testing.T) {
	st := testutil.NewTestStore(t)

	err := runConfiguredMuesliSync(context.Background(), st, config.MuesliSource{
		Identifier: "removed", AccountEmail: "you@example.com",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "add-muesli removed")
}

func TestFinishMuesliImportRefreshesCacheAfterPartialWrites(t *testing.T) {
	refreshed := 0
	err := finishMuesliImport("mac", &muesli.ImportSummary{MeetingsAdded: 1},
		errors.New("meeting 3 failed"), func() error { refreshed++; return nil })

	require.Error(t, err)
	assert.Equal(t, 1, refreshed)
	assert.Contains(t, err.Error(), "muesli sync mac failed")
}

func TestWriteMuesliSummaryReportsSkippedMeetings(t *testing.T) {
	var out bytes.Buffer

	writeMuesliSummary(&out, &muesli.ImportSummary{
		MeetingsProcessed: 3, MeetingsAdded: 2, SkippedDeleted: 4, SkippedEmpty: 1, SkippedInProgress: 1,
	})

	assert.Contains(t, out.String(), "Deleted in Muesli:  4 (kept archived)")
	assert.Contains(t, out.String(), "Empty:              1 (no notes or transcript)")
	assert.Contains(t, out.String(), "Still in progress:  1")
}

func TestWriteMuesliSummaryOmitsZeroSkipCounts(t *testing.T) {
	var out bytes.Buffer

	writeMuesliSummary(&out, &muesli.ImportSummary{MeetingsProcessed: 1, MeetingsAdded: 1})

	assert.NotContains(t, out.String(), "Deleted in Muesli")
	assert.NotContains(t, out.String(), "Empty:")
}

func TestWriteMuesliSummaryReportsContactsState(t *testing.T) {
	var out bytes.Buffer

	writeMuesliSummary(&out, &muesli.ImportSummary{ContactsState: muesli.ContactsUnavailable})

	assert.Contains(t, out.String(), "Contacts:           unavailable")
	assert.Contains(t, out.String(), "Full Disk Access")
}

func TestMuesliImportOptionsCarryContactsSettings(t *testing.T) {
	enabled := false
	opts := muesliImportOptions(config.MuesliSource{
		Identifier: "mac", AccountEmail: "you@example.com", DBPath: "/tmp/muesli.db",
		Contacts: &enabled, ContactsPath: "/tmp/AddressBook", PhoneCountryCode: "44",
	})

	assert.Equal(t, muesli.ImportOptions{
		Identifier: "mac", AccountEmail: "you@example.com", DBPath: "/tmp/muesli.db",
		ContactsEnabled: false, ContactsPath: "/tmp/AddressBook", PhoneCountryCode: "44",
	}, opts)
}
