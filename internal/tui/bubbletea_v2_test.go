package tui

import (
	"image/color"
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelImplementsBubbleTeaV2ModelAndUsesAltScreenView(t *testing.T) {
	m := New(nil, Options{})

	assert.Implements(t, (*tea.Model)(nil), m)

	view := m.View()
	assert.Equal(t, "Loading...", view.Content)
	assert.True(t, view.AltScreen)
}

func TestModelInitRequestsBubbleTeaBackgroundColor(t *testing.T) {
	m := New(nil, Options{})

	cmd := m.Init()
	require.NotNil(t, cmd)
	msg := cmd()
	cmds, ok := msg.(tea.BatchMsg)
	require.True(t, ok, "Init should batch startup commands")

	backgroundRequest := reflect.ValueOf(tea.RequestBackgroundColor).Pointer()
	var found bool
	for _, c := range cmds {
		if reflect.ValueOf(c).Pointer() == backgroundRequest {
			found = true
			break
		}
	}
	assert.True(t, found, "Init should request terminal background color through Bubble Tea")
}

func TestBackgroundColorMsgRebuildsAdaptiveStyles(t *testing.T) {
	m := New(nil, Options{})

	updated, _ := m.Update(tea.BackgroundColorMsg{Color: color.White})
	lightModel := asModel(t, updated)
	assert.Equal(t, rgba("#000000"), rgbaColor(lightModel.styles.titleBar.GetForeground()))

	updated, _ = m.Update(tea.BackgroundColorMsg{Color: color.Black})
	darkModel := asModel(t, updated)
	assert.Equal(t, rgba("#ffffff"), rgbaColor(darkModel.styles.titleBar.GetForeground()))
}

func rgba(hex string) rgbaValues {
	return rgbaColor(lipgloss.Color(hex))
}

func rgbaColor(c color.Color) rgbaValues {
	r, g, b, a := c.RGBA()
	return rgbaValues{r: r, g: g, b: b, a: a}
}

type rgbaValues struct {
	r uint32
	g uint32
	b uint32
	a uint32
}
