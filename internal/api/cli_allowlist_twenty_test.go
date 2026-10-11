package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTwentyCommandsAndSchedulerRouting(t *testing.T) {
	assert := assert.New(t)
	assert.True(cliRunCommandAllowed([]string{"add-twenty", "work"}))
	assert.True(cliRunCommandAllowed([]string{"sync-twenty", "work", "--full"}))
	name, ok := SchedulerJobNameForSource("twenty", "work")
	assert.True(ok)
	assert.Equal("twenty:work", name)
}
