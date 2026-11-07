package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublisherValidatesBeforeWriting(t *testing.T) {
	for _, test := range []struct{ content, rejection string }{
		{"Use `get_messagge`.", "unknown tool or guide field"},
		{"mva1_SYNTHETIC_NOT_A_TOKEN", "grant credential"},
		{"Bearer synthetic-not-a-key", "bearer credential"},
		{"person@invalid.invalid", "non-example email address"},
	} {
		content := test.content
		for _, check := range []bool{false, true} {
			t.Run(content+"/"+map[bool]string{false: "write", true: "check"}[check], func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				root := t.TempDir()
				source := filepath.Join(root, "guide.md")
				input := content + "\n" + beginTools + "\n" + endTools + "\n"
				requirements.NoError(os.WriteFile(source, []byte(input), 0o600))
				err := publishGuide(source, check)
				requirements.ErrorContains(err, test.rejection)
				assertions.NotContains(err.Error(), content, "refusals must not echo the rejected value")
				actual, err := os.ReadFile(source)
				requirements.NoError(err)
				assertions.Equal(input, string(actual))
			})
		}
	}
}

func TestPublisherAcceptsExamplePlaceholders(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	root := t.TempDir()
	source := filepath.Join(root, "guide.md")
	input := "Use `get_message` to read `body_returned`. alice@example.com alice@example.test mva1_... Bearer <key>\n" + beginTools + "\n" + endTools + "\n"
	requirements.NoError(os.WriteFile(source, []byte(input), 0o600))
	requirements.NoError(publishGuide(source, false))
	published, err := os.ReadFile(source)
	requirements.NoError(err)
	assertions.True(strings.HasPrefix(string(published), "Use `get_message`"))
	requirements.NoError(publishGuide(source, true))
}
