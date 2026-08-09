//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

type convergenceProgressPublisher struct {
	vector.DocumentPublisher

	progress vector.DocumentProgress
}

func (p *convergenceProgressPublisher) GetDocumentProgress(context.Context, vector.GenerationID) (vector.DocumentProgress, error) {
	return p.progress, nil
}

func TestContextualConvergenceCheckerRequiresExactJournalAndCompletedReconciliation(t *testing.T) {
	mainStore, err := store.Open(filepath.Join(t.TempDir(), "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainStore.Close() })
	require.NoError(t, mainStore.InitSchema())
	_, err = mainStore.DB().Exec(`UPDATE embedding_change_clock SET sequence = 12 WHERE singleton = 1`)
	require.NoError(t, err)

	publisher := &convergenceProgressPublisher{progress: vector.DocumentProgress{
		ChangeSequence: 12, ReconcileCursor: "done:12",
	}}
	checker := &contextualConvergenceChecker{
		legacy: &legacyConvergenceChecker{store: mainStore}, publisher: publisher,
	}
	state, err := checker.CheckConvergence(t.Context(), 3)
	require.NoError(t, err)
	assert.True(t, state.Complete())

	publisher.progress.ChangeSequence = 11
	state, err = checker.CheckConvergence(t.Context(), 3)
	require.NoError(t, err)
	assert.False(t, state.Complete(), "unconsumed journal must block activation")

	publisher.progress = vector.DocumentProgress{ChangeSequence: 12, ReconcileCursor: ""}
	state, err = checker.CheckConvergence(t.Context(), 3)
	require.NoError(t, err)
	assert.False(t, state.Complete(), "unfinished reconciliation must block activation")

	publisher.progress.ReconcileCursor = "done:11"
	state, err = checker.CheckConvergence(t.Context(), 3)
	require.NoError(t, err)
	assert.True(t, state.Complete(), "a completed full pass remains valid while later journal entries converge")
}

func TestConvergenceResultRefusesEachIncompleteDimension(t *testing.T) {
	complete := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		LatestJournalSequence:   4,
		ConsumedJournalSequence: 4,
		ReconciliationComplete:  true,
	}
	assert.True(t, complete.Complete())

	missing := complete
	missing.MessageCoverageComplete = false
	assert.False(t, missing.Complete())
	unconsumed := complete
	unconsumed.ConsumedJournalSequence = 3
	assert.False(t, unconsumed.Complete())
	unreconciled := complete
	unreconciled.ReconciliationComplete = false
	assert.False(t, unreconciled.Complete())
}

func TestRunEmbeddingsActivate_ContextualRequiresConvergenceUnlessForced(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	vectorPath := filepath.Join(dir, "vectors.db")
	c := config.NewDefaultConfig()
	c.Data.DataDir = dir
	c.Vector.Enabled = true
	c.Vector.DBPath = vectorPath
	c.Vector.Embeddings.APIFormat = vector.APIFormatVoyageContextual
	c.Vector.Embeddings.Endpoint = "https://example.invalid/v1"
	c.Vector.Embeddings.Model = "voyage-context-4"
	c.Vector.Embeddings.Dimension = 4
	withTestConfig(t, c)

	mainStore, err := store.Open(mainPath)
	require.NoError(t, err)
	require.NoError(t, mainStore.InitSchema())
	_, err = mainStore.DB().Exec(`UPDATE embedding_change_clock SET sequence = 2 WHERE singleton = 1`)
	require.NoError(t, err)
	require.NoError(t, sqlitevec.RegisterExtension())
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{
		Path: vectorPath, MainPath: mainPath, Dimension: 4, MainDB: mainStore.DB(),
	})
	require.NoError(t, err)
	gen, err := backend.CreateGeneration(t.Context(), c.Vector.Embeddings.Model, 4, c.Vector.GenerationFingerprint())
	require.NoError(t, err)
	require.NoError(t, backend.AdvanceDocumentChangeWatermark(t.Context(), gen, 1))
	require.NoError(t, backend.SetDocumentReconcileCursor(t.Context(), gen, "done:2"))
	require.NoError(t, backend.Close())
	require.NoError(t, mainStore.Close())

	oldYes, oldForce := embeddingsActivateYes, embeddingsActivateForce
	t.Cleanup(func() { embeddingsActivateYes, embeddingsActivateForce = oldYes, oldForce })
	embeddingsActivateYes = true
	embeddingsActivateForce = false
	cmd := embeddingsActivateCmd
	previousContext := cmd.Context()
	cmd.SetContext(t.Context())
	t.Cleanup(func() { cmd.SetContext(previousContext) })
	var output bytes.Buffer
	cmd.SetOut(&output)
	t.Cleanup(func() { cmd.SetOut(nil) })
	genArg := strconv.FormatInt(int64(gen), 10)

	err = runEmbeddingsActivate(cmd, []string{genArg})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "journal=1/2")

	embeddingsActivateForce = true
	require.NoError(t, runEmbeddingsActivate(cmd, []string{genArg}))
	assert.Contains(t, output.String(), "Generation "+genArg+" activated")
}

func setupManualContextualGeneration(t *testing.T, fingerprint string, retire bool) vector.GenerationID {
	t.Helper()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	vectorPath := filepath.Join(dir, "vectors.db")
	c := config.NewDefaultConfig()
	c.Data.DataDir = dir
	c.Vector.Enabled = true
	c.Vector.DBPath = vectorPath
	c.Vector.Embeddings.APIFormat = vector.APIFormatVoyageContextual
	c.Vector.Embeddings.Endpoint = "https://example.invalid/v1"
	c.Vector.Embeddings.Model = "voyage-context-4"
	c.Vector.Embeddings.Dimension = 4
	withTestConfig(t, c)
	if fingerprint == "" {
		fingerprint = c.Vector.GenerationFingerprint()
	}

	mainStore, err := store.Open(mainPath)
	require.NoError(t, err)
	require.NoError(t, mainStore.InitSchema())
	require.NoError(t, sqlitevec.RegisterExtension())
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{
		Path: vectorPath, MainPath: mainPath, Dimension: 4, MainDB: mainStore.DB(),
	})
	require.NoError(t, err)
	gen, err := backend.CreateGeneration(t.Context(), c.Vector.Embeddings.Model, 4, fingerprint)
	require.NoError(t, err)
	if retire {
		require.NoError(t, backend.RetireGeneration(t.Context(), gen, false))
	}
	require.NoError(t, backend.Close())
	require.NoError(t, mainStore.Close())

	oldYes, oldForce := embeddingsActivateYes, embeddingsActivateForce
	t.Cleanup(func() { embeddingsActivateYes, embeddingsActivateForce = oldYes, oldForce })
	embeddingsActivateYes = true
	embeddingsActivateForce = false
	previousContext := embeddingsActivateCmd.Context()
	embeddingsActivateCmd.SetContext(t.Context())
	t.Cleanup(func() { embeddingsActivateCmd.SetContext(previousContext) })
	return gen
}

func assertManualGenerationState(t *testing.T, gen vector.GenerationID, want vector.GenerationState) {
	t.Helper()
	db, rebind, closeDB, err := openEmbeddingsMetadataDB(t.Context())
	require.NoError(t, err)
	defer closeDB()
	row, err := getEmbeddingGeneration(t.Context(), db, rebind, gen)
	require.NoError(t, err)
	assert.Equal(t, want, row.State)
}

func TestRunEmbeddingsActivate_ContextualLifecycleRefusalsStayNonActivating(t *testing.T) {
	t.Run("wrong generation fingerprint", func(t *testing.T) {
		gen := setupManualContextualGeneration(t,
			"voyage-context-4:4:p1-111111:c32768:e1:avoyage-contextual:v0", false)
		err := runEmbeddingsActivate(embeddingsActivateCmd,
			[]string{strconv.FormatInt(int64(gen), 10)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match config")
		assertManualGenerationState(t, gen, vector.GenerationBuilding)
	})
	t.Run("retired generation", func(t *testing.T) {
		gen := setupManualContextualGeneration(t, "", true)
		err := runEmbeddingsActivate(embeddingsActivateCmd,
			[]string{strconv.FormatInt(int64(gen), 10)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), `is "retired", not "building"`)
		assertManualGenerationState(t, gen, vector.GenerationRetired)
	})
}
