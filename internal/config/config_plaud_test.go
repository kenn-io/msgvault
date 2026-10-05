package config

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlaudConfigRejectsInvalidSources(t *testing.T) {
	for _, tt := range []struct{ name, body, want string }{
		{"email required", `[[plaud]]
identifier="work"`, "requires account_email"},
		{"email invalid", `[[plaud]]
account_email="Display <owner@example.com>"`, "invalid account_email"},
		{"duplicate", `[[plaud]]
identifier="work"
account_email="owner@example.com"
[[plaud]]
identifier="WORK"
account_email="other@example.com"`, "duplicate identifier"},
		{"missing identifier", `[[plaud]]
account_email="owner@example.com"
[[plaud]]
identifier="work"
account_email="other@example.com"`, "every entry needs an identifier"},
		{"surrounding whitespace", `[[plaud]]
identifier=" work "
account_email="owner@example.com"`, "surrounding whitespace"},
		{"control character", `[[plaud]]
identifier="work\u0001"
account_email="owner@example.com"`, "unsafe identifier"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeMeetingConfig(t, tt.body), "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestPlaudConfigAcceptsPathLikeLabels(t *testing.T) {
	for _, identifier := range []string{"../work", `work\team`, ".", ".."} {
		t.Run(identifier, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			body := fmt.Sprintf("[[plaud]]\nidentifier=%q\naccount_email=\"owner@example.com\"", identifier)
			cfg, err := Load(writeMeetingConfig(t, body), "")
			require.NoError(err)
			assert.Equal(identifier, cfg.Plaud[0].Identifier)
		})
	}
}

func TestPlaudConfigDefaultsAndScheduling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg, err := Load(writeMeetingConfig(t, `[[plaud]]
account_email=" Owner@Example.COM "
enabled=true
schedule="0 */6 * * *"`), "")
	require.NoError(err)
	require.Len(cfg.Plaud, 1)
	assert.Equal("default", cfg.Plaud[0].Identifier)
	assert.Equal("owner@example.com", cfg.Plaud[0].AccountEmail)
	require.NotNil(cfg.GetPlaudSource("DEFAULT"))
	assert.Nil(cfg.GetPlaudSource("missing"))
	assert.Len(cfg.ScheduledPlaudSources(), 1)
	cfg.Plaud = append(cfg.Plaud, PlaudSource{Identifier: "disabled", Schedule: "0 * * * *"}, PlaudSource{Identifier: "manual", Enabled: true})
	assert.Len(cfg.ScheduledPlaudSources(), 1)
}
