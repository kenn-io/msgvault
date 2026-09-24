package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCLIRunEmbeddingsRejectsInternalWorkers(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"embeddings", "optimize"}))
	assert.False(t, cliRunCommandAllowed([]string{"embeddings", "__optimize-worker", "/tmp/other.db", "1", "1"}))
}
