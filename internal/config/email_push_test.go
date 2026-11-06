package config

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestEmailPushSourceIDsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		assertions := assert.New(ht)
		ids := hegel.Draw(ht, hegel.Lists(hegel.Integers[int64](math.MinInt64, math.MaxInt64)))
		valid := len(ids) <= 8
		seen := map[int64]bool{}
		for _, id := range ids {
			if id <= 0 || seen[id] {
				valid = false
			}
			seen[id] = true
		}
		err := (EmailPushConfig{IMAPSourceIDs: ids}).Validate()
		if valid {
			assertions.NoError(err)
		} else {
			assertions.Error(err)
		}
	}, hegel.WithTestCases(2000))
}

func TestEmailPushConfigDecoderRejectsInvalidSourceIDs(t *testing.T) {
	for _, values := range []string{"[0]", "[-1]", "[1, 1]", "[1,2,3,4,5,6,7,8,9]"} {
		t.Run(values, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			requirements.NoError(os.WriteFile(path, []byte("[email_push]\nimap_source_ids = "+values+"\n"), 0o600))
			snapshot, err := ReadConfigFile(path)
			requirements.NoError(err)
			_, err = LoadConfigFile(snapshot, dir)
			requirements.Error(err)
			assertions.Contains(err.Error(), "email_push.imap_source_ids")
		})
	}
}

func TestEmailPushDefaultOffAndLimits(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	cfg := NewDefaultConfig()
	assertions.Empty(cfg.EmailPush.IMAPSourceIDs)
	requirements.NoError(cfg.EmailPush.Validate())
	requirements.NoError((EmailPushConfig{IMAPSourceIDs: []int64{1, 2, 3, 4, 5, 6, 7, 8}}).Validate())
	requirements.Error((EmailPushConfig{IMAPSourceIDs: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9}}).Validate())
}
