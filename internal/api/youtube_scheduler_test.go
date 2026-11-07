package api

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestYouTubeSourceSchedulerClassification(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	name, ok := SchedulerJobNameForSource("youtube", "https://www.youtube.com/channel/UCfixture")
	requirements.True(ok)
	assertions.Equal("youtube:UCfixture", name)
	classification := classifySourceScheduling("youtube", "https://www.youtube.com/channel/UCfixture")
	assertions.Equal(sourceScheduleGeneric, classification.kind)
	assertions.Equal(name, classification.jobName)
	for _, id := range []string{"UCfixture", "https://evil.example/channel/UCfixture", "https://www.youtube.com/channel/UCfixture?token=x", "https://www.youtube.com/channel/"} {
		_, ok := SchedulerJobNameForSource("youtube", id)
		assertions.False(ok)
	}
}
