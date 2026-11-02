package cmd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func policyRuntimeFixture(t *testing.T) (*store.Store, *config.Config, context.Context, int64) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[conversation_policy]\nactivity_window_days = 1\n[beeper]\nmax_participants = 7\n"), 0600))
	cfg, err := config.Load(path, dir)
	require.NoError(t, err)
	cfg.Server.APIPort = freeTCPPort(t)
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Analytics.AutoBuildCache = false
	cfg.Vector.Enabled = false
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		connection, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		var database, schema string
		require.NoError(t, st.DB().QueryRow(`SELECT current_database(),current_schema()`).Scan(&database, &schema))
		connection.Path = "/" + database
		query := connection.Query()
		query.Set("search_path", schema+",public")
		connection.RawQuery = query.Encode()
		cfg.Data.DatabaseURL = connection.String()
	} else {
		require.NoError(t, st.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&cfg.Data.DatabaseURL))
	}
	src, err := st.GetOrCreateSource("beeper", "quiet-account")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "quiet-room", "Synthetic quiet room")
	require.NoError(t, err)
	return st, cfg, testInvocationContext(t.Context(), cfg, invocationOptions{}), room
}

func TestServeConversationPolicyBeforeConsumers(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, cfg, ctx, room := policyRuntimeFixture(t)
	starts := observePolicyConsumers(t)
	// A prior staged archive must be bootstrapped even with no scheduled source.
	_, err := st.DB().Exec(`UPDATE conversation_policy SET effective_tier='full'`)
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(ctx)
	done := make(chan error, 1)
	go func() { done <- runServe(cmd, nil) }()
	waitForServeHealth(t, cfg.Server.APIPort, done)
	for range 3 {
		select {
		case <-starts:
		case <-time.After(60 * time.Second):
			requirements.FailNow("consumer did not start")
		}
	}
	rows, err := st.ListConversationPolicies(ctx)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(room, rows[0].ConversationID)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier)
	cancel()
	select {
	case err := <-done:
		requirements.NoError(err)
	case <-time.After(20 * time.Second):
		requirements.FailNow("daemon did not stop")
	}
}

func TestConversationPolicyRuntimeMalformedReloadStillExpires(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, cfg, ctx, room := policyRuntimeFixture(t)
	now := time.Now().UTC()
	_, err := st.UpsertMessage(&store.Message{SourceID: 1, ConversationID: room, SourceMessageID: "own", MessageType: "beeper", IsFromMe: true, SentAt: sql.NullTime{Time: now, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(initializeConversationPolicy(ctx, st, cfg, now))
	before, err := st.ConversationPolicySettingsContext(ctx)
	requirements.NoError(err)
	requirements.NoError(os.WriteFile(cfg.ConfigFilePath(), []byte("[broken"), 0600))
	ticks := make(chan time.Time)
	observed := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runConversationPolicyRuntime(ctx, st, currentConversationPolicySettingsSource(invocationFromContext(ctx)), ticks, func(err error) { observed <- err })
	}()
	ticks <- now.Add(24*time.Hour + time.Minute)
	select {
	case err := <-observed:
		requirements.Error(err)
	case <-time.After(20 * time.Second):
		requirements.FailNow("policy pass did not finish")
	}
	rows, err := st.ListConversationPolicies(ctx)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier)
	after, err := st.ConversationPolicySettingsContext(ctx)
	requirements.NoError(err)
	assertions.Equal(before, after)
	cancel()
	<-done
}

func TestServeConversationPolicyBootstrapFailureBlocksConsumers(t *testing.T) {
	requirements := require.New(t)
	st, _, ctx, _ := policyRuntimeFixture(t)
	starts := observePolicyConsumers(t)
	// The configuration is valid; the archive rejects the policy write.
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_policy_bootstrap() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.key LIKE 'conversation_policy%' THEN RAISE EXCEPTION 'synthetic policy bootstrap failure'; END IF; RETURN NEW; END $$`)
		requirements.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER fail_policy_bootstrap BEFORE INSERT OR UPDATE ON archive_metadata FOR EACH ROW EXECUTE FUNCTION fail_policy_bootstrap()`)
		requirements.NoError(err)
	} else {
		for _, event := range []string{"INSERT", "UPDATE"} {
			_, err := st.DB().Exec(`CREATE TRIGGER fail_policy_bootstrap_` + strings.ToLower(event) + ` BEFORE ` + event + ` ON archive_metadata
				WHEN NEW.key LIKE 'conversation_policy%' BEGIN SELECT RAISE(ABORT, 'synthetic policy bootstrap failure'); END`)
			requirements.NoError(err)
		}
	}
	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(ctx)
	requirements.ErrorContains(runServe(cmd, nil), "configure conversation policy")
	assert.Empty(t, starts)
}

func TestConversationPolicyRuntimeConfigReloadAndOverrides(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, cfg, ctx, _ := policyRuntimeFixture(t)
	now := time.Now().UTC()
	requirements.NoError(initializeConversationPolicy(ctx, st, cfg, now))
	requirements.NoError(st.SetConversationPolicyOverride(ctx, 1, "quiet-room", conversationpolicy.Cold))
	body := "[beeper]\nmax_participants = 0\n[conversation_policy]\nactivity_window_days = 9\n[[conversation_policy.overrides]]\nsource_type = 'beeper'\nsource_identifier = 'quiet-account'\nconversation_id = 'quiet-room'\ntier = 'full'\n"
	requirements.NoError(os.WriteFile(cfg.ConfigFilePath(), []byte(body), 0600))
	source := currentConversationPolicySettingsSource(invocationFromContext(ctx))
	requirements.NoError(refreshConversationPolicyFromSource(ctx, st, source, now))
	settings, err := st.ConversationPolicySettingsContext(ctx)
	requirements.NoError(err)
	assertions.Zero(settings.MaxParticipants["beeper"])
	assertions.Equal(9, settings.ActivityWindowDays)
	rows, err := st.ListConversationPolicies(ctx)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier, "persisted CLI override wins")
	requirements.NoError(st.SetConversationPolicyOverride(ctx, 1, "quiet-room", ""))
	requirements.NoError(refreshConversationPolicyFromSource(ctx, st, source, now))
	rows, err = st.ListConversationPolicies(ctx)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier)
	// Reopen the actual archive to verify persisted settings and effective state.
	reopened, err := store.Open(cfg.DatabaseDSN())
	requirements.NoError(err)
	defer func() { _ = reopened.Close() }()
	persisted, err := reopened.ConversationPolicySettingsContext(ctx)
	requirements.NoError(err)
	assertions.Equal(settings, persisted)
	rows, err = reopened.ListConversationPolicies(ctx)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier)
}

func TestConversationPolicyPreviewPreservesArchive(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, cfg, ctx, room := policyRuntimeFixture(t)
	message, err := st.UpsertMessage(&store.Message{SourceID: 1, ConversationID: room, SourceMessageID: "preview-own", MessageType: "beeper", IsFromMe: true, SentAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}})
	requirements.NoError(err)
	addBlob := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		hash := hex.EncodeToString(sum[:])
		relative := hash[:2] + "/" + hash
		path := filepath.Join(cfg.AttachmentsDir(), relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
		require.NoError(t, st.UpsertAttachment(message, hash+".bin", "application/octet-stream", relative, hash, len(content)))
		return hash
	}
	packedHash := addBlob("synthetic packed preview content")
	maintenance, err := newAttachmentMaintenance(st, cfg.AttachmentsDir(), nil, true)
	requirements.NoError(err)
	packed, err := maintenance.pack(ctx, 0)
	requirements.NoError(err)
	requirements.Equal(1, packed.BlobsPacked)
	requirements.NoError(maintenance.close())
	addBlob("synthetic loose preview content")
	files := func() map[string][32]byte {
		result := make(map[string][32]byte)
		require.NoError(t, filepath.WalkDir(cfg.AttachmentsDir(), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			result[path] = sha256.Sum256(content)
			return nil
		}))
		return result
	}
	beforeFiles := files()
	packBefore, err := st.GetAttachmentPackEntry(packedHash)
	requirements.NoError(err)
	metadata := func() map[string]string {
		result := make(map[string]string)
		rows, err := st.DB().Query(`SELECT key,value FROM archive_metadata`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var key, value string
			require.NoError(t, rows.Scan(&key, &value))
			result[key] = value
		}
		require.NoError(t, rows.Err())
		return result
	}
	beforeMetadata := metadata()
	_, err = st.DB().Exec(`UPDATE conversation_policy SET effective_tier='full'`)
	requirements.NoError(err)
	before, err := st.ListConversationPolicies(ctx)
	requirements.NoError(err)
	revision, err := st.ConversationPolicyRevision(ctx)
	requirements.NoError(err)
	preview := newPurgeExcludedMediaLocalCmd(defaultPurgeExcludedMediaDeps(ctx))
	preview.SetArgs([]string{"--dry-run"})
	requirements.NoError(preview.ExecuteContext(ctx))
	after, err := st.ListConversationPolicies(ctx)
	requirements.NoError(err)
	assertions.Equal(before, after)
	afterRevision, err := st.ConversationPolicyRevision(ctx)
	requirements.NoError(err)
	assertions.Equal(revision, afterRevision)
	assertions.Equal(beforeMetadata, metadata(), "preview must not drain dirty hints or write metadata")
	assertions.Equal(beforeFiles, files(), "preview must not remove loose blobs or repack sealed files")
	packAfter, err := st.GetAttachmentPackEntry(packedHash)
	requirements.NoError(err)
	assertions.Equal(packBefore, packAfter)
}

func TestConversationPolicyEndToEndScratchArchive(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, _, ctx, _ := policyRuntimeFixture(t)
	_, err := st.DB().Exec(`UPDATE conversation_policy SET effective_tier='full'`)
	requirements.NoError(err)
	writer, closeWriter, err := openWritableStoreAndInitForIngestInvocation(invocationFromContext(ctx))
	requirements.NoError(err)
	rows, err := writer.ListConversationPolicies(ctx)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier, "standalone writer bootstraps before admitting messages or workers")
	closeWriter()
	rows, err = st.ListConversationPolicies(ctx)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier)
}

// Wrap the real production consumer factories; assert archive state before each
// factory performs any reads, then continue through the actual implementation.
func observePolicyConsumers(t *testing.T) <-chan string {
	t.Helper()
	starts := make(chan string, 3)
	check := func(ctx context.Context, st *store.Store, name string) {
		rows, err := st.ListConversationPolicies(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, conversationpolicy.Cold, rows[0].EffectiveTier, name)
		starts <- name
	}
	oldAnalytics, oldPeople, oldVector := prepareServeAnalyticsEngine, registerServePersonEnrichmentJob, startServeVectorInit
	t.Cleanup(func() {
		prepareServeAnalyticsEngine = oldAnalytics
		registerServePersonEnrichmentJob = oldPeople
		startServeVectorInit = oldVector
	})
	prepareServeAnalyticsEngine = func(ctx context.Context, cfg *config.Config, st *store.Store, intent startupCacheBuildIntent) (query.Engine, string, startupCacheBuildOutcome, bool, error) {
		check(ctx, st, "analytics")
		return oldAnalytics(ctx, cfg, st, intent)
	}
	registerServePersonEnrichmentJob = func(ctx context.Context, sched *scheduler.Scheduler, st *store.Store, cfg personenrichment.Config, credentials personEnrichmentRuntimeCredentials) error {
		check(ctx, st, "people")
		return oldPeople(ctx, sched, st, cfg, credentials)
	}
	startServeVectorInit = func(ctx context.Context, st *store.Store, path string, tracker scheduler.WorkTracker, server *api.Server, sched *scheduler.Scheduler, openers ...visual.StreamOpener) *vectorInitHandle {
		check(ctx, st, "vector")
		return oldVector(ctx, st, path, tracker, server, sched, openers...)
	}
	return starts
}

// A broken config reload must not freeze self attribution. Removing the self
// identity that kept a room full has to take effect on the next minute tick,
// using the last validated settings.
func TestConversationPolicyRuntimeMalformedReloadReattributesSelf(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, cfg, _, _ := policyRuntimeFixture(t)
	requirements.NoError(os.WriteFile(cfg.ConfigFilePath(), []byte("[conversation_policy]\nactivity_window_days = 1\n[slack]\nmax_participants = 7\n"), 0600))
	reloaded, err := config.Load(cfg.ConfigFilePath(), cfg.Data.DataDir)
	requirements.NoError(err)
	reloaded.Data.DatabaseURL = cfg.Data.DatabaseURL
	ctx := testInvocationContext(t.Context(), reloaded, invocationOptions{})
	source, err := st.GetOrCreateSource("slack", "team:self")
	requirements.NoError(err)
	room, err := st.EnsureConversation(source.ID, "large-room", "Synthetic large room")
	requirements.NoError(err)
	self, err := st.EnsureParticipant("self@example.test", "Self", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(source.ID, "self@example.test", "manual"))
	now := time.Now().UTC()
	message, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "mention", MessageType: "slack", SentAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(message, "mention", []int64{self}, []string{""}))
	requirements.NoError(initializeConversationPolicy(ctx, st, reloaded, now))
	tier := func() conversationpolicy.Tier {
		rows, err := st.ListConversationPolicies(ctx)
		requirements.NoError(err)
		for _, row := range rows {
			if row.ConversationID == room {
				return row.EffectiveTier
			}
		}
		requirements.FailNow("missing policy row")
		return ""
	}
	requirements.Equal(conversationpolicy.Full, tier(), "a recent mention of the user keeps the room full")

	requirements.NoError(os.WriteFile(reloaded.ConfigFilePath(), []byte("[broken"), 0600))
	_, err = st.RemoveAccountIdentity(source.ID, "self@example.test")
	requirements.NoError(err)
	err = refreshConversationPolicyFromSource(ctx, st, currentConversationPolicySettingsSource(invocationFromContext(ctx)), now.Add(time.Minute))
	requirements.Error(err, "the reload failure is still reported")
	assertions.Equal(conversationpolicy.Cold, tier(), "the removed identity no longer counts as the user's activity")
}
