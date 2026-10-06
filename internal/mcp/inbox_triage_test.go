package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestInboxTriageCatalogSeparatesPreviewAndWriteAdmission(t *testing.T) {
	// Discovery must register tools without contacting the daemon. Using its
	// real client also proves that each interface can be admitted independently.
	client, err := daemonclient.New(daemonclient.Config{URL: "http://127.0.0.1:1", APIKey: "synthetic-owner-key", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	for _, tc := range []struct {
		name                   string
		preview, apply, writes bool
	}{
		{"missing", false, false, true},
		{"preview-only", true, false, true},
		{"apply-only", false, true, true},
		{"read-only", true, true, false},
		{"both", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := ServeOptions{CalendarOnly: true}
			if tc.preview {
				opts.InboxTriagePreview = client
			}
			if tc.apply {
				opts.InboxTriageApply = client
			}
			tools := toolsByName(t, rawListTools(t, opts, tc.writes))
			_, preview := tools["inbox_triage_preview"]
			_, apply := tools["inbox_triage_apply"]
			assert.Equal(t, tc.preview, preview)
			assert.Equal(t, tc.apply && tc.writes, apply)
			if preview {
				assert.Equal(t, true, toolReadOnlyHint(t, tools["inbox_triage_preview"]))
			}
			if apply {
				assert.Equal(t, false, toolReadOnlyHint(t, tools["inbox_triage_apply"]))
			}
		})
	}
}
