//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// fakeEmbeddingsServer answers OpenAI-compatible /embeddings requests with
// one deterministic dim-4 vector per input, recording how many inputs it was
// asked to embed.
func fakeEmbeddingsServer(t *testing.T, embedded *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		*embedded += len(req.Input)
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		payload := struct {
			Data  []item `json:"data"`
			Model string `json:"model"`
		}{Model: "test-model"}
		for i := range req.Input {
			payload.Data = append(payload.Data, item{Embedding: []float32{1, 0, 0, 0}, Index: i})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedTwoAccountMainDB builds a real main DB (via InitSchema) with one
// message+body in each of two accounts.
func seedTwoAccountMainDB(t *testing.T, dataDir string) {
	t.Helper()
	s, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	require.NoError(t, err, "open main db")
	defer func() { require.NoError(t, s.Close()) }()
	require.NoError(t, s.InitSchema(), "InitSchema")
	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'a@example.com'), (2, 'gmail', 'b@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread'), (2, 2, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, subject) VALUES
	(1, 1, 1, 'a1', 'email', 'hello from a'),
	(2, 2, 2, 'b1', 'email', 'hello from b');
INSERT INTO message_bodies (message_id, body_text) VALUES (1, 'body a1'), (2, 'body b1');
`)
	require.NoError(t, err, "seed corpus")
}

func embedGenByID(t *testing.T, dataDir string) map[int64]sql.NullInt64 {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "msgvault.db"))
	require.NoError(t, err, "open main db handle")
	defer func() { require.NoError(t, db.Close()) }()
	rows, err := db.Query(`SELECT id, embed_gen FROM messages ORDER BY id`)
	require.NoError(t, err, "read embed_gen")
	defer func() { _ = rows.Close() }()
	out := map[int64]sql.NullInt64{}
	for rows.Next() {
		var id int64
		var gen sql.NullInt64
		require.NoError(t, rows.Scan(&id, &gen))
		out[id] = gen
	}
	require.NoError(t, rows.Err())
	return out
}

// TestRunEmbed_AccountScopedBuild drives the full runEmbed path (real
// sqlitevec backend, real store, fake embeddings endpoint): a build scoped
// to one account embeds and stamps only that account's messages and
// activates, and a follow-up run scoped differently is refused by the
// fingerprint mismatch with the scope visible in the error.
func TestRunEmbed_AccountScopedBuildActivatesScopedGeneration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var embedded int
	srv := fakeEmbeddingsServer(t, &embedded)

	dataDir := t.TempDir()
	seedTwoAccountMainDB(t, dataDir)

	oldCfg := cfg
	c := &config.Config{}
	c.Vector.Enabled = true
	c.Vector.DBPath = filepath.Join(dataDir, "vectors.db")
	c.Vector.Embeddings.Endpoint = srv.URL
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 4
	c.Data.DataDir = dataDir
	cfg = c

	oldRebuild, oldYes := embedFullRebuild, embedYes
	oldAccounts, oldCollections := embedAccounts, embedCollections
	oldBackstop := embedBackstop
	t.Cleanup(func() {
		cfg = oldCfg
		embedFullRebuild, embedYes = oldRebuild, oldYes
		embedAccounts, embedCollections = oldAccounts, oldCollections
		embedBackstop = oldBackstop
	})
	embedYes = true
	embedBackstop = false

	newCmd := func() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
		cmd.SetOut(out)
		cmd.SetErr(errOut)
		return cmd, out, errOut
	}

	// Run 1: full rebuild scoped to account A.
	embedFullRebuild = true
	embedAccounts = []string{"a@example.com"}
	cmd, out, errOut := newCmd()
	require.NoError(runEmbeddingsBuildLocal(cmd), "scoped full rebuild")
	assert.Contains(errOut.String(), "Embedding scope: src-1", "scope printed to stderr")
	assert.Contains(errOut.String(), "add equivalent accounts to [vector.embed.scope]", "one-off scope warns how to persist its generation")
	assert.Contains(out.String(), "Generation 1 activated.", "scoped generation activates")
	assert.Equal(1, embedded, "only account A's message reached the embedding endpoint")

	gens := embedGenByID(t, dataDir)
	require.True(gens[1].Valid, "account A message stamped")
	assert.Equal(int64(1), gens[1].Int64)
	assert.False(gens[2].Valid, "account B message keeps embed_gen NULL")

	// Run 2: same archive, scoped to account B instead — the fingerprint
	// difference must refuse to top up the src-1 generation.
	embedFullRebuild = false
	embedAccounts = []string{"b@example.com"}
	cmd, _, _ = newCmd()
	err := runEmbeddingsBuildLocal(cmd)
	require.Error(err, "a different account scope must not resume the src-1 generation")
	assert.Contains(err.Error(), "src-", "stored vs configured scope is visible in the error")
	assert.Contains(err.Error(), "full-rebuild", "error points at --full-rebuild")
}

// TestRunEmbedLivePersonGateStopsLaterBatchesAfterConfigDeletion drives the
// complete manual build path. Deleting config after the first curated-person
// request must prevent every later person request without rolling back message
// progress or stranding activation of the completed message generation.
func TestRunEmbedLivePersonGateStopsLaterBatchesAfterConfigDeletion(t *testing.T) {
	var messageRequests atomic.Int32
	var personRequests atomic.Int32
	var configRemoved atomic.Bool
	removeResult := make(chan error, 1)
	dataDir := t.TempDir()

	configured := config.NewDefaultConfig()
	configured.HomeDir = dataDir
	configured.Data.DataDir = dataDir
	configured.Vector.Enabled = true
	configured.Vector.DBPath = filepath.Join(dataDir, "vectors.db")
	configured.Vector.Embeddings.Model = "manual-live-gate-model"
	configured.Vector.Embeddings.Dimension = 4
	configured.Vector.Embeddings.BatchSize = 1
	configured.Vector.Embeddings.MaxRetries = 1
	configured.Vector.People = vector.PeopleConfig{
		Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		for _, input := range request.Input {
			if strings.HasPrefix(input, "Subject:") {
				messageRequests.Add(1)
				continue
			}
			personRequests.Add(1)
			if configRemoved.CompareAndSwap(false, true) {
				removeResult <- os.Remove(configured.ConfigFilePath())
			}
		}
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[i] = map[string]any{"embedding": []float32{1, 0, 0, 0}, "index": i}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": data, "model": "manual-live-gate-model",
		})
	}))
	t.Cleanup(provider.Close)
	configured.Vector.Embeddings.Endpoint = provider.URL
	withTestConfig(t, configured)
	require.NoError(t, configured.Save())

	seedTwoAccountMainDB(t, dataDir)
	mainStore, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	require.NoError(t, err)
	profile, err := configured.Vector.SemanticPersonEmbeddingProfile()
	require.NoError(t, err)
	_, err = mainStore.EnsurePersonSemanticEmbeddingProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = mainStore.GrantPersonSemanticEmbeddingConsent(
		t.Context(), profile.Fingerprint, "test",
	)
	require.NoError(t, err)
	for _, personSeed := range []struct {
		email string
		name  string
	}{
		{email: "first-curated@example.test", name: "First Curated Person"},
		{email: "second-curated@example.test", name: "Second Curated Person"},
	} {
		participantID, err := mainStore.EnsureParticipantByIdentifier(
			"email", personSeed.email, "Observed "+personSeed.name,
		)
		require.NoError(t, err)
		person, _, err := mainStore.CreatePersonFromParticipantContext(t.Context(), participantID)
		require.NoError(t, err)
		_, err = mainStore.UpdatePersonDisplayNameContext(
			t.Context(), person.ID, person.Revision, &personSeed.name,
		)
		require.NoError(t, err)
	}
	require.NoError(t, mainStore.Close())

	oldRebuild, oldYes := embedFullRebuild, embedYes
	oldAccounts, oldCollections := embedAccounts, embedCollections
	oldBackstop := embedBackstop
	t.Cleanup(func() {
		embedFullRebuild, embedYes = oldRebuild, oldYes
		embedAccounts, embedCollections = oldAccounts, oldCollections
		embedBackstop = oldBackstop
	})
	embedFullRebuild = true
	embedYes = true
	embedBackstop = false
	embedAccounts = nil
	embedCollections = nil
	command := &cobra.Command{}
	command.SetContext(t.Context())
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	command.SetOut(stdout)
	command.SetErr(stderr)

	err = runEmbeddingsBuildLocal(command)

	require.NoError(t, err, stderr.String())
	require.True(t, configRemoved.Load(), "precondition: first curated-person request removed config")
	require.NoError(t, <-removeResult)
	assert.Contains(t, stdout.String(), "Generation 1 activated.",
		"person authorization loss must not strand completed message activation")

	// A later manual resume must still process newly arrived message work from
	// the startup configuration while the live person policy remains absent.
	mainStore, err = store.Open(filepath.Join(dataDir, "msgvault.db"))
	require.NoError(t, err)
	_, err = mainStore.DB().Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id, message_type, subject)
		VALUES (3, 1, 1, 'post-removal-message', 'email', 'post-removal message');
		INSERT INTO message_bodies (message_id, body_text)
		VALUES (3, 'post-removal body');`)
	require.NoError(t, err)
	require.NoError(t, mainStore.Close())
	embedFullRebuild = false
	resume := &cobra.Command{}
	resume.SetContext(t.Context())
	resumeOut, resumeErr := &bytes.Buffer{}, &bytes.Buffer{}
	resume.SetOut(resumeOut)
	resume.SetErr(resumeErr)
	require.NoError(t, runEmbeddingsBuildLocal(resume), resumeErr.String())

	assert.Equal(t, int32(3), messageRequests.Load(),
		"message batches must continue through the separate ungated client after config removal")
	assert.Equal(t, int32(1), personRequests.Load(),
		"config deletion must fence every later curated-person batch")
}

// TestRunEmbed_AccountScopedRebuildRefusesEmptyScope guards the empty-scope
// activation hazard: a full rebuild scoped to an account that exists but has
// no live messages (added but never synced) would immediately reach
// remaining == 0 and activate an empty generation, retiring the working
// index. The run must fail instead of activating.
func TestRunEmbed_AccountScopedRebuildRefusesEmptyScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var embedded int
	srv := fakeEmbeddingsServer(t, &embedded)

	dataDir := t.TempDir()
	seedTwoAccountMainDB(t, dataDir)
	s, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	require.NoError(err, "open main db")
	_, err = s.DB().Exec(`INSERT INTO sources (id, source_type, identifier) VALUES (3, 'gmail', 'c@example.com')`)
	require.NoError(err, "seed empty account")
	require.NoError(s.Close())

	oldCfg := cfg
	c := &config.Config{}
	c.Vector.Enabled = true
	c.Vector.DBPath = filepath.Join(dataDir, "vectors.db")
	c.Vector.Embeddings.Endpoint = srv.URL
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 4
	c.Data.DataDir = dataDir
	cfg = c

	oldRebuild, oldYes := embedFullRebuild, embedYes
	oldAccounts, oldCollections := embedAccounts, embedCollections
	oldBackstop := embedBackstop
	t.Cleanup(func() {
		cfg = oldCfg
		embedFullRebuild, embedYes = oldRebuild, oldYes
		embedAccounts, embedCollections = oldAccounts, oldCollections
		embedBackstop = oldBackstop
	})
	embedYes = true
	embedBackstop = false
	embedFullRebuild = true
	embedAccounts = []string{"c@example.com"}

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)

	err = runEmbeddingsBuildLocal(cmd)
	require.Error(err, "an empty account scope must not activate")
	assert.Contains(err.Error(), "0 live messages")
	assert.NotContains(out.String(), "activated", "no generation may activate")
	assert.Zero(embedded, "no message text may reach the embedding endpoint")
}
