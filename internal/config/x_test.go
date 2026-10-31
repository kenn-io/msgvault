package config

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadXTestConfig(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	cfg := NewDefaultConfig()
	cfg.HomeDir = dir
	cfg.Data.DataDir = dir
	path := cfg.ConfigFilePath()
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return Load(path, dir)
}

func TestXPolicyPreservesZeroBudget(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int64
	}{
		{"explicit zero", "[[x.accounts]]\nuser_id='1000000001'\nmonthly_budget=0\n", 0},
		{"missing budget", "[[x.accounts]]\nuser_id='1000000001'\n", 5_000_000},
		{"registered without override", "[[accounts]]\nemail='1000000001'\nenabled=true\n", 5_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadXTestConfig(t, tc.body)
			require.NoError(t, err)
			got, err := cfg.XAccount("1000000001")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.MonthlyBudgetMicros)
			if tc.want == 0 {
				assert.Zero(t, got.MonthlyBudgetMicros)
			}
			assert.Equal(t, "1000000001", got.UserID)
			assert.False(t, got.OwnedReads)
			assert.False(t, got.AllowSend)
			assert.Equal(t, 100, got.RefreshBatchSize)
			assert.Equal(t, 24*time.Hour, got.RefreshSweepInterval)
			assert.False(t, got.PostReadPriceOverride)
			assert.Equal(t, int64(5_000), got.PostReadPriceMicros)
			assert.Equal(t, int64(10_000), got.UserReadPriceMicros)
			assert.Equal(t, int64(200_000), got.CreateReservationMicros)
		})
	}
}

func TestParseUSDMicros(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"0", 0}, {"5.00", 5_000_000}, {"0.005", 5_000},
		{"0.000001", 1}, {"1.0000000", 1_000_000},
		{"5e-3", 5_000}, {"1E3", 1_000_000_000},
		{"9223372036854.775807", math.MaxInt64},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, err := ParseUSDMicros(tc.value)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	for _, value := range []string{"", " ", "-1", "-0.000001", "NaN", "nan", "+inf", "-inf", "Infinity", "0.0000001", "0.0000010000000000000001", "9223372036854.775808", "9223372036855", "1e999999999999", "1e-999999999999", "0x10", "1/2", "1 0", ".5", "5.", "1_000", "１", "1e"} {
		t.Run("invalid/"+value, func(t *testing.T) { _, err := ParseUSDMicros(value); require.Error(t, err) })
	}
}

func TestXPolicyRejectsInvalidConfig(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"duplicate", "[[x.accounts]]\nuser_id='1000000001'\n"},
		{"empty id", "user_id=''"}, {"missing id", "owned_reads=true"},
		{"username", "user_id='example'"}, {"leading zero", "user_id='01'"},
		{"zero id", "user_id='0'"}, {"negative id", "user_id='-1'"},
		{"numeric id", "user_id=1000000001"}, {"unicode id", "user_id='１２'"},
		{"overlong id", "user_id='" + strings.Repeat("1", 129) + "'"},
		{"batch zero", "refresh_batch_size=0"}, {"batch negative", "refresh_batch_size=-1"}, {"batch oversized", "refresh_batch_size=101"},
		{"interval zero", "refresh_sweep_interval='0s'"}, {"interval negative", "refresh_sweep_interval='-1h'"}, {"interval invalid", "refresh_sweep_interval='daily'"},
		{"create too small", "create_reservation=0.199999"},
		{"unknown key", "allow_sned=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "[[x.accounts]]\n"
			if tc.name != "missing id" && !strings.Contains(tc.body, "user_id") {
				body += "user_id='1000000001'\n"
			}
			body += tc.body
			if tc.name == "duplicate" {
				body = "[[x.accounts]]\nuser_id='1000000001'\n" + tc.body
			}
			_, err := loadXTestConfig(t, body)
			require.Error(t, err)
		})
	}
	for _, key := range []string{"monthly_budget", "post_read_price", "user_read_price", "create_reservation"} {
		for _, value := range []string{"-1", "nan", "inf", "-inf", "1e30", "0.0000001", "0.0000010000000000000001", "9223372036854.775808", "true", "[]"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				_, err := loadXTestConfig(t, fmt.Sprintf("[[x.accounts]]\nuser_id='1000000001'\n%s=%s\n", key, value))
				require.Error(t, err)
			})
		}
	}
}

func TestXAccountSelectorValidation(t *testing.T) {
	cfg, err := loadXTestConfig(t, "[[x.accounts]]\nuser_id='1000000001'\n[[x.sends]]\nuser_id='1000000001'\nenabled=true\n")
	require.NoError(t, err)
	other, err := cfg.XAccount("1000000002")
	require.NoError(t, err)
	assert.False(t, other.AllowSend)
	account, err := cfg.XAccount("1000000001")
	require.NoError(t, err)
	require.Error(t, account.Validate("1000000002"))
	for _, id := range []string{"", "0", "01", "@example", "1000000001 "} {
		_, err = cfg.XAccount(id)
		require.Error(t, err)
	}
	cfg.X.Accounts = append(cfg.X.Accounts, account)
	_, err = cfg.XAccount("1000000001")
	require.Error(t, err)
}

func TestXPolicyExactTOMLRoundTrip(t *testing.T) {
	cfg, err := loadXTestConfig(t, `
[slack]
max_participants=7
[discord]
enabled=true
[[x.accounts]]
user_id='9007199254740993'
monthly_budget=9223372036854.775807
owned_reads=true
refresh_batch_size=17
refresh_sweep_interval='12h'
post_read_price=5e-3
user_read_price=0.010_001
create_reservation=0.250001
[[x.accounts]]
user_id='1000000002'
monthly_budget=0
post_read_price=0
user_read_price=0
[[x.sends]]
user_id='9007199254740993'
enabled=true
`)
	require.NoError(t, err)
	got, err := cfg.XAccount("9007199254740993")
	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), got.MonthlyBudgetMicros)
	assert.Equal(t, int64(5000), got.PostReadPriceMicros)
	assert.Equal(t, int64(10001), got.UserReadPriceMicros)
	assert.Equal(t, int64(250001), got.CreateReservationMicros)
	assert.True(t, got.OwnedReads)
	assert.True(t, got.AllowSend)
	assert.Equal(t, 17, got.RefreshBatchSize)
	assert.Equal(t, 12*time.Hour, got.RefreshSweepInterval)
	zero, err := cfg.XAccount("1000000002")
	require.NoError(t, err)
	assert.Zero(t, zero.MonthlyBudgetMicros)
	assert.Zero(t, zero.PostReadPriceMicros)
	assert.Zero(t, zero.UserReadPriceMicros)
	require.NoError(t, cfg.Save())
	reloaded, err := Load(cfg.ConfigFilePath(), cfg.HomeDir)
	require.NoError(t, err)
	assert.Equal(t, cfg.X, reloaded.X)
	assert.Equal(t, cfg.Slack, reloaded.Slack)
	assert.Equal(t, cfg.Discord, reloaded.Discord)
	assert.Equal(t, cfg.Beeper, reloaded.Beeper)
}

func TestXPolicyTOMLFormsPreserveDecimalText(t *testing.T) {
	for _, body := range []string{
		`[[x.accounts]]
user_id='1000000001'
monthly_budget=0.000001
post_read_price=0.005
`,
		`[x]
accounts=[{user_id='1000000001',monthly_budget=0.000001,post_read_price=0.005}]
`,
		`x.accounts=[{user_id='1000000001',monthly_budget=0.000001,post_read_price=0.005}]
`,
		`[["x".'accounts']]
'user_id'='1000000001'
"monthly\u005fbudget"=0.000001
'post_read_price'=0.005
`,
		`ignored = '''
monthly_budget=999999999999999999999999999
" = '  # irrelevant string content
'''
# monthly_budget=999999999999999999999999999
[[x.accounts]]
user_id='1000000001' # inline comment
monthly_budget=0.000001
post_read_price='0.005'
`,
	} {
		cfg, err := loadXTestConfig(t, body)
		require.NoError(t, err)
		account, err := cfg.XAccount("1000000001")
		require.NoError(t, err)
		assert.Equal(t, int64(1), account.MonthlyBudgetMicros)
		assert.Equal(t, int64(5000), account.PostReadPriceMicros)
	}
	for _, value := range []string{`'0_.005'`, `'0.0000010000000000000001'`} {
		_, err := loadXTestConfig(t, "[[x.accounts]]\nuser_id='1000000001'\npost_read_price="+value)
		require.Error(t, err)
	}
	cfg, err := loadXTestConfig(t, "[[x.accounts]]\nuser_id='"+strings.Repeat("1", 128)+"'\n")
	require.NoError(t, err)
	_, err = cfg.XAccount(strings.Repeat("1", 128))
	require.NoError(t, err)
}

func TestXAccountRejectsInvalidRuntimePolicies(t *testing.T) {
	cfg := NewDefaultConfig()
	account, err := cfg.XAccount("1000000001")
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		change func(*XAccountConfig)
	}{
		{"budget", func(a *XAccountConfig) { a.MonthlyBudgetMicros = -1 }},
		{"post price", func(a *XAccountConfig) { a.PostReadPriceMicros = -1 }},
		{"user price", func(a *XAccountConfig) { a.UserReadPriceMicros = -1 }},
		{"create minimum", func(a *XAccountConfig) { a.CreateReservationMicros = 199999 }},
		{"batch zero", func(a *XAccountConfig) { a.RefreshBatchSize = 0 }},
		{"batch too large", func(a *XAccountConfig) { a.RefreshBatchSize = 101 }},
		{"interval", func(a *XAccountConfig) { a.RefreshSweepInterval = 0 }},
		{"owner", func(a *XAccountConfig) { a.UserID = "1000000002" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := account
			tc.change(&policy)
			require.Error(t, policy.Validate("1000000001"))
			cfg.X.Accounts = []XAccountConfig{policy}
			if tc.name != "owner" {
				_, err := cfg.XAccount("1000000001")
				require.Error(t, err)
			}
		})
	}
}

func TestXPostReadPriceOverrideSurvivesRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, price string
		want        int64
		override    bool
	}{
		{"implicit", "", 5000, false},
		{"explicit default", "post_read_price=0.005", 5000, true},
		{"custom", "post_read_price=0.007001", 7001, true},
		{"explicit zero", "post_read_price=0", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadXTestConfig(t, "[[x.accounts]]\nuser_id='1000000001'\nowned_reads=true\n"+tc.price)
			require.NoError(t, err)
			policy, err := cfg.XAccount("1000000001")
			require.NoError(t, err)
			assert.Equal(t, tc.override, policy.PostReadPriceOverride)
			assert.Equal(t, tc.want, policy.PostReadPriceMicros)
			assert.True(t, policy.OwnedReads)
			require.NoError(t, cfg.Save())
			reloaded, err := Load(cfg.ConfigFilePath(), cfg.HomeDir)
			require.NoError(t, err)
			policy, err = reloaded.XAccount("1000000001")
			require.NoError(t, err)
			assert.Equal(t, tc.override, policy.PostReadPriceOverride)
			assert.Equal(t, tc.want, policy.PostReadPriceMicros)
			assert.True(t, policy.OwnedReads)
		})
	}
}
