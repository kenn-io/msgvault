package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			cfg, err := loadXTestConfig(t, tc.body)
			_testRequire.NoError(err)
			policy, err := cfg.XAccount("1001")
			_testRequire.NoError(err)
			_testAssert.Equal(tc.want, policy.AllowSend)
			_testAssert.Equal(int64(5000000), policy.MonthlyBudgetMicros)
			_testRequire.NoError(cfg.Save())
			again, err := Load(cfg.ConfigFilePath(), cfg.HomeDir)
			_testRequire.NoError(err)
			restored, err := again.XAccount("1001")
			_testRequire.NoError(err)
			_testAssert.Equal(tc.want, restored.AllowSend)
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
