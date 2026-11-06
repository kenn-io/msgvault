package config

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"hegel.dev/go/hegel"
)

func TestKeepInTouchConfigDefaultsAndExplicitZeroWindow(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	defaults := NewDefaultConfig().People.KeepInTouch
	assert.False(defaults.Enabled)
	assert.Equal("0 9 * * *", defaults.Schedule)
	assert.Empty(defaults.Owner)
	assert.Equal(7, defaults.ApproachDays)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[people.keep_in_touch]\nenabled = true\nowner = 'operator'\napproach_days = 0\nlimit = 4\nmax_drafts_per_run = 2\n"), 0o600))
	cfg, err := Load(path, "")
	require.NoError(err)
	assert.True(cfg.People.KeepInTouch.Enabled)
	assert.Zero(cfg.People.KeepInTouch.ApproachDays)
	assert.Equal(4, cfg.People.KeepInTouch.Limit)
}
func TestKeepInTouchConfigWindowDomain(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		days := hegel.Draw(ht, hegel.Integers(math.MinInt, math.MaxInt))
		cfg := NewDefaultConfig().People.KeepInTouch
		cfg.ApproachDays = days
		Assert.Equal(ht, days < 0 || days > 3660, cfg.Validate() != nil)
	})
}
