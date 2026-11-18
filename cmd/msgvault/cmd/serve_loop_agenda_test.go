package cmd

import (
	"net"
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

func TestLoopAgendaOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr net.Addr
		want string
	}{
		{"loopback", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}, "http://127.0.0.1:8080"},
		{"named address", &net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 8080}, "http://192.0.2.10:8080"},
		{"ipv6", &net.TCPAddr{IP: net.IPv6loopback, Port: 8080}, "http://[::1]:8080"},
		{"every ipv4 interface", &net.TCPAddr{IP: net.IPv4zero, Port: 8080}, ""},
		{"every ipv6 interface", &net.TCPAddr{IP: net.IPv6unspecified, Port: 8080}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, loopAgendaOrigin(tc.addr))
		})
	}
}
