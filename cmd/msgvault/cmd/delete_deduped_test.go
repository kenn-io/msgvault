package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

// TestDeleteDeduped_NeitherFlag verifies that omitting both --batch and
// --all-hidden produces an error mentioning both flag names.
func TestDeleteDeduped_NeitherFlag(t *testing.T) {
	var batch []string
	var allHidden bool
	cmd := &cobra.Command{Use: "delete-test", SilenceErrors: true}
	sub := &cobra.Command{
		Use: "delete-deduped",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(batch) == 0 && !allHidden {
				return errors.New("must specify --batch or --all-hidden")
			}
			return nil
		},
	}
	sub.Flags().StringArrayVar(&batch, "batch", nil, "")
	sub.Flags().BoolVar(&allHidden, "all-hidden", false, "")
	sub.MarkFlagsMutuallyExclusive("batch", "all-hidden")
	cmd.AddCommand(sub)
	cmd.SetArgs([]string{"delete-deduped"})

	err := cmd.Execute()
	require.Error(t, err, "expected error when neither --batch nor --all-hidden is set")
	msg := err.Error()
	assert.Contains(t, msg, "--batch", "error should mention --batch flag name")
	assert.Contains(t, msg, "--all-hidden", "error should mention --all-hidden flag name")
}

// TestDeleteDeduped_MutualExclusion verifies that passing both --batch and
// --all-hidden is rejected by cobra.
func TestDeleteDeduped_MutualExclusion(t *testing.T) {
	var batch []string
	var allHidden bool
	cmd := &cobra.Command{Use: "delete-test", SilenceErrors: true}
	sub := &cobra.Command{Use: "delete-deduped", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	sub.Flags().StringArrayVar(&batch, "batch", nil, "")
	sub.Flags().BoolVar(&allHidden, "all-hidden", false, "")
	sub.MarkFlagsMutuallyExclusive("batch", "all-hidden")
	cmd.AddCommand(sub)
	cmd.SetArgs([]string{"delete-deduped", "--batch", "some-id", "--all-hidden"})

	err := cmd.Execute()
	require.Error(t, err, "expected error when both --batch and --all-hidden are set")
	msg := err.Error()
	assert.Contains(t, msg, "batch", "error should mention batch flag name")
	assert.Contains(t, msg, "all-hidden", "error should mention all-hidden flag name")
	_ = batch
	_ = allHidden
}

func TestDeleteDedupedUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	var planRequests atomic.Int32
	var executeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cli/delete-deduped/plan":
			assert.Equal(t, http.MethodPost, r.Method, "plan method")
			var req struct {
				BatchIDs  []string `json:"batch_ids"`
				AllHidden bool     `json:"all_hidden"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode plan request") {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			assert.Equal(t, []string{"batch-a", "batch-b"}, req.BatchIDs, "plan batch ids")
			assert.False(t, req.AllHidden, "plan all_hidden")
			planRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"total": 3,
				"batch_count": 2,
				"batches": [
					{"id": "batch-a", "count": 2},
					{"id": "batch-b", "count": 1}
				]
			}`))
		case "/api/v1/cli/delete-deduped":
			assert.Equal(t, http.MethodPost, r.Method, "execute method")
			var req struct {
				BatchIDs           []string `json:"batch_ids"`
				AllHidden          bool     `json:"all_hidden"`
				NoBackup           bool     `json:"no_backup"`
				ExpectedTotal      int64    `json:"expected_total"`
				ExpectedBatchCount int64    `json:"expected_batch_count"`
				ExpectedBatches    []struct {
					ID    string `json:"id"`
					Count int64  `json:"count"`
				} `json:"expected_batches"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode execute request") {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			assert.Equal(t, []string{"batch-a", "batch-b"}, req.BatchIDs, "execute batch ids")
			assert.False(t, req.AllHidden, "execute all_hidden")
			assert.True(t, req.NoBackup, "execute no_backup")
			assert.Equal(t, int64(3), req.ExpectedTotal, "execute expected_total")
			assert.Equal(t, int64(2), req.ExpectedBatchCount, "execute expected_batch_count")
			if assert.Len(t, req.ExpectedBatches, 2, "execute expected_batches") {
				assert.Equal(t, "batch-a", req.ExpectedBatches[0].ID, "expected batch-a id")
				assert.Equal(t, int64(2), req.ExpectedBatches[0].Count, "expected batch-a count")
				assert.Equal(t, "batch-b", req.ExpectedBatches[1].ID, "expected batch-b id")
				assert.Equal(t, int64(1), req.ExpectedBatches[1].Count, "expected batch-b count")
			}
			executeRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"deleted": 3, "batch_count": 2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote: config.RemoteConfig{
			URL:           server.URL,
			AllowInsecure: true,
		},
	})
	oldLogger := logger
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	t.Cleanup(func() { logger = oldLogger })

	oldBatchIDs := deleteDedupedBatchIDs
	oldAllHidden := deleteDedupedAllHidden
	oldNoBackup := deleteDedupedNoBackup
	oldYes := deleteDedupedYes
	t.Cleanup(func() {
		deleteDedupedBatchIDs = oldBatchIDs
		deleteDedupedAllHidden = oldAllHidden
		deleteDedupedNoBackup = oldNoBackup
		deleteDedupedYes = oldYes
	})
	deleteDedupedBatchIDs = []string{"batch-a", "batch-b"}
	deleteDedupedAllHidden = false
	deleteDedupedNoBackup = true
	deleteDedupedYes = true

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: "delete-deduped", RunE: runDeleteDeduped}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "delete-deduped command")

	assert.Equal(t, int32(1), planRequests.Load(), "plan request count")
	assert.Equal(t, int32(1), executeRequests.Load(), "execute request count")
	assert.Empty(t, stderr.String(), "stderr")
	assert.Equal(t, `Will permanently delete 3 hidden message(s) from 2 batch(es):
  batch-a: 2 row(s)
  batch-b: 1 row(s)

Deleted 3 message(s) from 2 batch(es).

Caches may have stale entries; rebuild each separately:
  'msgvault build-cache --full-rebuild'        (parquet analytics)
  'msgvault embeddings build --full-rebuild'   (vector index, if enabled)
`, stdout.String())
}
