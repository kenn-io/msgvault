package config

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestXSendOptInUsesNumericOwner(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"omitted", "", false},
		{"enabled", "[[x.sends]]\nuser_id='1001'\nenabled=true\n", true},
		{"disabled", "[[x.sends]]\nuser_id='1001'\nenabled=false\n", false},
		{"wrong owner", "[[x.sends]]\nuser_id='2001'\nenabled=true\n", false},
		{"enabled omitted", "[[x.sends]]\nuser_id='1001'\n", false},
		{"legacy false is no gate", "[[x.accounts]]\nuser_id='1001'\nallow_send=false\n[[x.sends]]\nuser_id='1001'\nenabled=true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadXTestConfig(t, tc.body)
			require.NoError(t, err)
			policy, err := cfg.XAccount("1001")
			require.NoError(t, err)
			assert.Equal(t, tc.want, policy.AllowSend)
			assert.Equal(t, int64(5000000), policy.MonthlyBudgetMicros)
			require.NoError(t, cfg.Save())
			again, err := Load(cfg.ConfigFilePath(), cfg.HomeDir)
			require.NoError(t, err)
			restored, err := again.XAccount("1001")
			require.NoError(t, err)
			assert.Equal(t, tc.want, restored.AllowSend)
		})
	}
}
func TestXSendOptInRejectsAmbiguityAndObsoleteEnablement(t *testing.T) {
	for _, body := range []string{
		"[[x.sends]]\nsource_id=1\nenabled=true\n",
		"[[x.sends]]\nuser_id='01'\nenabled=true\n",
		"[[x.sends]]\nuser_id='1001'\nenabled=true\n[[x.sends]]\nuser_id='1001'\nenabled=false\n",
		"[[x.sends]]\nuser_id='1001'\nenabled='true'\n",
		"[[x.accounts]]\nuser_id='1001'\nallow_send=true\n",
	} {
		_, err := loadXTestConfig(t, body)
		require.Error(t, err)
	}
}
