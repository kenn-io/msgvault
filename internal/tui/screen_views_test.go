package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestScreenViewsFollowPresentationAndUserActivity(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	path := filepath.Join(t.TempDir(), "synthetic.mbox")
	requirements.NoError(os.WriteFile(path, []byte("From sender@example.com Mon Jan 1 12:00:00 +0000 2024\r\nFrom: Sender <sender@example.com>\r\nTo: Reader <reader@example.com>\r\nSubject: Synthetic screen visit\r\n\r\nSynthetic body.\r\n"), 0o600))
	_, err := importer.ImportMbox(t.Context(), st, path, importer.MboxImportOptions{SourceType: "mbox", Identifier: "synthetic@example.com"})
	requirements.NoError(err)
	m := New(query.NewEngine(st.DB(), st.IsPostgreSQL()), Options{TextEngine: meetingModeTextEngine{}, PeopleBackend: &fakePeopleBackend{}})
	m.width, m.height = 100, 24
	screens := []string{}
	m.reportScreen = func(ctx context.Context, screen string) error {
		deadline, ok := ctx.Deadline()
		assertions.True(ok)
		assertions.WithinDuration(time.Now().Add(3*time.Second), deadline, time.Second)
		screens = append(screens, screen)
		return nil
	}
	update := func(msg tea.Msg) {
		updated, command := m.Update(msg)
		m = asModel(t, updated)
		if command != nil {
			_ = runBatchCommand(t, command)
		}
	}
	update(tea.BackgroundColorMsg{})
	requirements.Equal([]string{"email"}, screens)
	update(tea.BackgroundColorMsg{})
	assertions.Len(screens, 1)
	for range 3 {
		update(key('m'))
	}
	assertions.Equal([]string{"email", "texts", "meetings", "directory"}, screens)
	m.transitionBuffer = "frozen directory"
	m.mode = modeEmail
	update(tea.BackgroundColorMsg{})
	assertions.Equal("directory", m.reportedScreen)
	m.mode = modePeople
	m.transitionBuffer = ""
	update(key(','))
	assertions.True(m.settings.active)
	assertions.Equal("settings", screens[len(screens)-1])
	update(keyEsc())
	assertions.Equal("directory", screens[len(screens)-1])
	m.reportedDay = "2020-01-01"
	update(tea.BackgroundColorMsg{})
	assertions.Len(screens, 6)
	update(keyDown())
	assertions.Len(screens, 7)
}
