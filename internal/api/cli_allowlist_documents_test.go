package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCLIRunCommandAllowedDocumentMutations(t *testing.T) {
	for _, subcommand := range []string{
		"build", "consent-mistral", "purge-derived", "resume", "retire", "retry",
	} {
		t.Run(subcommand, func(t *testing.T) {
			assert.True(t, cliRunCommandAllowed([]string{"documents", subcommand}))
		})
	}

	for _, subcommand := range []string{"probe-mistral", "search", "status"} {
		t.Run(subcommand, func(t *testing.T) {
			assert.False(t, cliRunCommandAllowed([]string{"documents", subcommand}))
		})
	}
}
