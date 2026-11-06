package api

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/msgvault/internal/sourcescheduling"
)

func TestPocketDaemonRouting(t *testing.T) {
	assertions := assert.New(t)
	assertions.True(cliRunCommandAllowed([]string{"add-pocket", "personal"}))
	assertions.True(cliRunCommandAllowed([]string{"sync-pocket", "personal", "--limit", "1"}))
	name, ok := sourcescheduling.SchedulerJobNameForSource("pocket", "personal")
	assertions.True(ok)
	assertions.Equal("pocket:personal", name)
}
