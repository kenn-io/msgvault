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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	stage := testRecord()
	installed, err := stage.WithConsumedStage(stage)
	_testRequire.NoError(err)
	manager := NewTokenManager(tokenTempDir(t))
	_testRequire.NoError(manager.Save(t.Context(), installed))
	loaded, err := manager.Load(t.Context(), stage.OwnerID)
	_testRequire.NoError(err)
	copied := stage
	copied.Generation = 999
	copied.Username = "renamed_example"
	copied.Expiry = time.Now().Add(24 * time.Hour)
	copied.GrantedScopes = slices.Clone(stage.GrantedScopes)
	slices.Reverse(copied.GrantedScopes)
	consumed, err := loaded.ConsumesStage(copied)
	_testRequire.NoError(err)
	_testAssert.True(consumed)
	fresh := NewTokenRecord(stage.OwnerID, stage.ClientID, stage.ClientType, "fresh-access", "fresh-refresh", "")
	fresh.GrantedScopes = stage.GrantedScopes
	fresh.Expiry = stage.Expiry
	consumed, err = loaded.ConsumesStage(fresh)
	_testRequire.NoError(err)
	_testAssert.False(consumed)
	narrowed := stage
	narrowed.GrantedScopes = []string{"tweet.read"}
	consumed, err = loaded.ConsumesStage(narrowed)
	_testRequire.NoError(err)
	_testAssert.False(consumed)
	public, err := json.Marshal(loaded)
	_testRequire.NoError(err)
	_testAssert.NotContains(string(public), "consumed_stage")
	_testAssert.NotContains(string(public), stage.RefreshToken())
}

func TestConsumedStageCorruptFileFailsClosed(t *testing.T) {
	_testRequire := require.New(t)

	dir := tokenTempDir(t)
	manager := NewTokenManager(dir)
	record := testRecord()
	_testRequire.NoError(manager.Save(t.Context(), record))
	path := filepath.Join(dir, "x_"+record.OwnerID+".json")
	data, err := os.ReadFile(path)
	_testRequire.NoError(err)
	var stored map[string]any
	_testRequire.NoError(json.Unmarshal(data, &stored))
	stored["consumed_stage"] = "invalid-marker"
	data, err = json.Marshal(stored)
	_testRequire.NoError(err)
	_testRequire.NoError(fileutil.SecureReplaceFile(path, data, 0600))
	_, err = manager.Load(t.Context(), record.OwnerID)
	_testRequire.Error(err)
	assert.NotContains(t, err.Error(), record.RefreshToken())
}
