package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

// TestCreateSubsetVCardResourcesRequireFlag runs the command against a real
// archive holding a native vCard body that names a person outside the subset,
// so the flag is proven where it matters: on what lands in the shared file.
func TestCreateSubsetVCardResourcesRequireFlag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()

	dataDir := t.TempDir()
	testCfg := lifecycleTestConfig(dataDir)
	withStoreResolverConfig(t, testCfg)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err, "open test archive")
	require.NoError(st.InitSchema(), "init test archive")
	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	senderID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(
		src.ID, "thread-1", "email_thread", "Thread 1")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "msg-1",
		MessageType:     "email",
		SenderID:        sql.NullInt64{Int64: senderID, Valid: true},
		SentAt: sql.NullTime{
			Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true,
		},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(
		messageID, "from", []int64{senderID}, []string{"Bob"}))
	person, _, err := st.CreatePersonFromParticipant(senderID)
	require.NoError(err)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Bob Example\r\n" +
		"RELATED;TYPE=colleague:urn:uuid:outsider\r\nEND:VCARD\r\n")
	envelope, err := vcard.ParseResourceEnvelope(raw)
	require.NoError(err)
	envelope.SourceRef = "address-book"
	envelope.SourceResourceUID = "source-bob"
	envelope.CanonicalPersonUID = person.VCardUID
	_, err = st.PutVCardResourceEnvelopeContext(ctx, store.VCardResourceEnvelopeInput{
		PersonID: person.ID, Envelope: envelope,
	})
	require.NoError(err)
	_, err = st.RetirePersonUIDAliasContext(ctx, "retired-bob", &person.ID, "merge")
	require.NoError(err)
	require.NoError(st.Close(), "close test archive")

	oldRows, oldOutput := subsetRows, subsetOutput
	oldProfiles, oldResources := subsetIncludeProfiles, subsetIncludeVCardResources
	t.Cleanup(func() {
		subsetRows, subsetOutput = oldRows, oldOutput
		subsetIncludeProfiles, subsetIncludeVCardResources = oldProfiles, oldResources
	})
	subsetRows = 1

	openSubset := func(dir string) *store.Store {
		subset, err := store.Open(filepath.Join(dir, "msgvault.db"))
		require.NoError(err, "open subset archive")
		t.Cleanup(func() { _ = subset.Close() })
		return subset
	}

	flag := createSubsetCmd.Flags().Lookup("include-vcard-resources")
	require.NotNil(flag, "the opt-in must be reachable from the CLI")
	assert.Equal("false", flag.DefValue, "native vCard resources stay private by default")

	subsetOutput = filepath.Join(t.TempDir(), "profiles")
	subsetIncludeProfiles, subsetIncludeVCardResources = true, false
	profilesStderr := captureStderrDuring(t, func() {
		require.NoError(runCreateSubset(&cobra.Command{Use: "create-subset"}, nil))
	})
	assert.NotContains(profilesStderr, "--include-vcard-resources",
		"an unset opt-in must not warn about vCard bodies")
	profiles := openSubset(subsetOutput)
	_, err = profiles.GetVCardResourceEnvelopeContext(ctx, "address-book", "source-bob")
	require.ErrorIs(err, store.ErrVCardResourceNotFound,
		"--include-profiles alone must not copy opaque vCard bodies")
	_, err = profiles.ResolveRetiredPersonUIDContext(ctx, "retired-bob")
	require.ErrorIs(err, store.ErrPersonUIDAliasNotFound)

	// The bodies cannot be copied without the profiles they project into,
	// and the command must say so up front rather than warn about copying
	// them and then copy nothing.
	subsetOutput = filepath.Join(t.TempDir(), "orphan-resources")
	subsetIncludeProfiles, subsetIncludeVCardResources = false, true
	orphanStderr := captureStderrDuring(t, func() {
		err := runCreateSubset(&cobra.Command{Use: "create-subset"}, nil)
		require.ErrorContains(err, "--include-vcard-resources requires --include-profiles")
	})
	assert.NotContains(orphanStderr, "WARNING: --include-vcard-resources",
		"a refused run must not announce a copy it will not make")
	_, err = os.Stat(filepath.Join(subsetOutput, "msgvault.db"))
	require.ErrorIs(err, os.ErrNotExist, "a refused run must create no archive")

	subsetOutput = filepath.Join(t.TempDir(), "resources")
	subsetIncludeProfiles, subsetIncludeVCardResources = true, true
	resourcesStderr := captureStderrDuring(t, func() {
		require.NoError(runCreateSubset(&cobra.Command{Use: "create-subset"}, nil))
	})
	assert.Contains(resourcesStderr, "WARNING: --include-vcard-resources",
		"the opt-in must state what it exposes before copying it")
	resources := openSubset(subsetOutput)
	copied, err := resources.GetVCardResourceEnvelopeContext(
		ctx, "address-book", "source-bob")
	require.NoError(err)
	assert.Equal(raw, copied.OriginalRawBytes)
	alias, err := resources.ResolveRetiredPersonUIDContext(ctx, "retired-bob")
	require.NoError(err)
	require.NotNil(alias.SurvivingPersonID)
	assert.Equal(person.ID, *alias.SurvivingPersonID)
}
