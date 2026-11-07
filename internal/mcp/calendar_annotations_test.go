package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalendarToolAnnotations(t *testing.T) {
	// Listing the production catalog needs no provider call. The fake backend
	// makes the calendar capability available without accounts or credentials.
	tools := toolsByName(t, rawListTools(t, ServeOptions{
		Calendar: &calendarMCPFake{}, DelegatedOnly: true, AllowCalendarWrites: true,
	}, true))
	require.Len(t, tools, 7)
	for _, test := range []struct {
		name        string
		readOnly    bool
		destructive bool
	}{
		{"calendar_create", false, false},
		{"calendar_update", false, true},
		{"calendar_delete", false, true},
		{"calendar_move", false, true},
		{"calendar_respond", false, true},
		{"calendar_freebusy", true, false},
		{"calendar_conflicts", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool := tools[test.name]
			require.NotNil(t, tool)
			annotations, ok := tool["annotations"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, true, annotations["openWorldHint"])
			assert.Equal(t, test.readOnly, annotations["readOnlyHint"])
			assert.Equal(t, test.destructive, annotations["destructiveHint"])
		})
	}
}
