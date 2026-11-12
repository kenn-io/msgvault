package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/clirun"
)

func TestThreadsOAuthHandoffCommandRestriction(t *testing.T) {
	assert := assert.New(t)
	srv := &Server{}
	for _, command := range []string{"sync", "add-inline"} {
		assert.False(srv.cliRunEnvAllowedForCommand([]string{command}, clirun.EnvThreadsOAuth), command)
	}
	assert.False(srv.cliRunEnvAllowedForCommand(nil, clirun.EnvThreadsOAuth))
}
