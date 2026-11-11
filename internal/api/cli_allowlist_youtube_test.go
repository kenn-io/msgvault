package api

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestYouTubeCLIRunCommandAllowed(t *testing.T) {
	for _, action := range []string{"add", "sync"} {
		assert.True(t, cliRunCommandAllowed([]string{"source", "youtube", action, "example"}))
	}
}
