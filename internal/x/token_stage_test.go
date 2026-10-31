package x

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fileutil"
)

func TestConsumedStageIdentityIgnoresLocalCountersAndClocks(t *testing.T) {
	stage := testRecord()
	installed, err := stage.WithConsumedStage(stage)
	require.NoError(t, err)
	manager := NewTokenManager(tokenTempDir(t))
	require.NoError(t, manager.Save(t.Context(), installed))
	loaded, err := manager.Load(t.Context(), stage.OwnerID)
	require.NoError(t, err)
	copied := stage
	copied.Generation = 999
	copied.Username = "renamed_example"
	copied.Expiry = time.Now().Add(24 * time.Hour)
	copied.GrantedScopes = slices.Clone(stage.GrantedScopes)
	slices.Reverse(copied.GrantedScopes)
	consumed, err := loaded.ConsumesStage(copied)
	require.NoError(t, err)
	assert.True(t, consumed)
	fresh := NewTokenRecord(stage.OwnerID, stage.ClientID, stage.ClientType, "fresh-access", "fresh-refresh", "")
	fresh.GrantedScopes = stage.GrantedScopes
	fresh.Expiry = stage.Expiry
	consumed, err = loaded.ConsumesStage(fresh)
	require.NoError(t, err)
	assert.False(t, consumed)
	narrowed := stage
	narrowed.GrantedScopes = []string{"tweet.read"}
	consumed, err = loaded.ConsumesStage(narrowed)
	require.NoError(t, err)
	assert.False(t, consumed)
	public, err := json.Marshal(loaded)
	require.NoError(t, err)
	assert.NotContains(t, string(public), "consumed_stage")
	assert.NotContains(t, string(public), stage.RefreshToken())
}

func TestConsumedStageCorruptFileFailsClosed(t *testing.T) {
	dir := tokenTempDir(t)
	manager := NewTokenManager(dir)
	record := testRecord()
	require.NoError(t, manager.Save(t.Context(), record))
	path := filepath.Join(dir, "x_"+record.OwnerID+".json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var stored map[string]any
	require.NoError(t, json.Unmarshal(data, &stored))
	stored["consumed_stage"] = "invalid-marker"
	data, err = json.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, fileutil.SecureReplaceFile(path, data, 0600))
	_, err = manager.Load(t.Context(), record.OwnerID)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), record.RefreshToken())
}
