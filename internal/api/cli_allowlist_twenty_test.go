package api

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/msgvault/internal/sourcescheduling"
)

func TestTwentyCommandsAndSchedulerRouting(t *testing.T) {
	assert := assert.New(t)
	assert.True(cliRunCommandAllowed([]string{"add-twenty", "work"}))
	assert.True(cliRunCommandAllowed([]string{"sync-twenty", "work", "--full"}))
	name, ok := sourcescheduling.SchedulerJobNameForSource("twenty", "work")
	assert.True(ok)
	assert.Equal("twenty:work", name)
}
