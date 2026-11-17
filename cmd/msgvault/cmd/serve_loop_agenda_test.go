package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
)

func TestRegisterLoopAgendaJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.TaskIntegrationConfig
		want bool
	}{
		{"disabled", config.TaskIntegrationConfig{}, false},
		{"default on", config.TaskIntegrationConfig{Enabled: true}, true},
		{"explicit on", config.TaskIntegrationConfig{Enabled: true, AutoAgenda: new(true)}, true},
		{"explicit off", config.TaskIntegrationConfig{Enabled: true, AutoAgenda: new(false)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sched := scheduler.New(nil)
			require.NoError(t, registerLoopAgendaJob(sched, tc.cfg, nil))
			assert.Equal(t, tc.want, sched.IsJobScheduled("loops-agenda"))
		})
	}
}
