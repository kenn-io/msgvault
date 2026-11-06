package config

import (
	"bytes"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// The interval contract covers the entire duration domain, including overflow
// boundaries, fractional seconds and disabled accounts with invalid values.
func TestAccountPollingDurationDomain(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		enabled := hegel.Draw(ht, hegel.Booleans())
		d := time.Duration(n)
		_, err := loadPollFixture(ht.T, fmt.Sprintf(`[[accounts]]
email = "sample@example.test"
schedule = "0 * * * *"
enabled = %t
poll_interval = %q
`, enabled, d.String()))
		if d == 0 || (d >= 15*time.Second && d <= 120*time.Second && d%time.Second == 0) {
			assert.NoError(ht, err)
		} else {
			assert.ErrorContains(ht, err, "poll_interval")
		}
	})
}

func TestAccountPollingAcceptedDurationsSurviveSave(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		seconds := hegel.Draw(ht, hegel.Integers(15, 120))
		cfg, err := loadPollFixture(ht.T, fmt.Sprintf(`[[accounts]]
email = "sample@example.test"
schedule = "0 * * * *"
enabled = true
poll_interval = "%ds"
`, seconds))
		require.NoError(ht, err)
		require.NoError(ht, cfg.Save())
		reloaded, err := Load(cfg.configPath, cfg.HomeDir)
		require.NoError(ht, err)
		var encoded bytes.Buffer
		require.NoError(ht, toml.NewEncoder(&encoded).Encode(reloaded))
		var result struct {
			Accounts []struct {
				Interval string `toml:"poll_interval"`
			} `toml:"accounts"`
		}
		_, err = toml.Decode(encoded.String(), &result)
		require.NoError(ht, err)
		require.Len(ht, result.Accounts, 1)
		duration, err := time.ParseDuration(result.Accounts[0].Interval)
		require.NoError(ht, err)
		assert.Equal(ht, time.Duration(seconds)*time.Second, duration)
	})
}
