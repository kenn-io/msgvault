package peoplesweep

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRemovedDocumentSourceWarnsOnce(t *testing.T) {
	warnedDocumentText.Store(false)
	t.Cleanup(func() { warnedDocumentText.Store(false) })
	var warnings bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&warnings, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for range 2 {
		cfg := Config{Providers: map[string]ProviderConfig{"default": {
			AllowedSources: []SourceClass{SourceConversationText, SourceDocumentText},
		}}}
		cfg.ApplyDefaults()
	}
	assert.Equal(t, 1, strings.Count(warnings.String(), "document_text was removed; drop it from allowed_sources"))
}
