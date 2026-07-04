package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
)

func TestModelImplementsBubbleTeaV2ModelAndUsesAltScreenView(t *testing.T) {
	m := New(nil, Options{})

	assert.Implements(t, (*tea.Model)(nil), m)

	view := m.View()
	assert.Equal(t, "Loading...", view.Content)
	assert.True(t, view.AltScreen)
}
