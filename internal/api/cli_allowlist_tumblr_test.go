package api

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestTumblrCLIAllowlist(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"import-tumblr", "--blog", "orchard-example", "/export"}))
}
