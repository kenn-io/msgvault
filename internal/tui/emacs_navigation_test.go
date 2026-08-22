package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/query"
)

func TestCtrlPNNavigateEveryVerticalSurface(t *testing.T) {
	type surface struct {
		name   string
		setup  func(start int) Model
		apply  func(t *testing.T, model Model, key tea.KeyPressMsg) Model
		cursor func(Model) int
	}

	rows := []query.AggregateRow{{Key: "one"}, {Key: "two"}, {Key: "three"}}
	messages := []query.MessageSummary{{ID: 1}, {ID: 2}, {ID: 3}}
	surfaces := []surface{
		{
			name: "aggregate list",
			setup: func(start int) Model {
				model := NewBuilder().WithRows(rows...).WithPageSize(5).Build()
				model.cursor = start
				return model
			},
			apply: func(t *testing.T, model Model, key tea.KeyPressMsg) Model {
				return applyAggregateKey(t, model, key)
			},
			cursor: func(model Model) int { return model.cursor },
		},
		{
			name: "text conversation list",
			setup: func(start int) Model {
				model := NewBuilder().WithPageSize(5).Build()
				model.mode = modeTexts
				model.textState.level = textLevelConversations
				model.textState.conversations = []query.ConversationRow{{ConversationID: 1}, {ConversationID: 2}, {ConversationID: 3}}
				model.textState.cursor = start
				return model
			},
			apply: func(t *testing.T, model Model, key tea.KeyPressMsg) Model {
				updated, _ := model.handleTextListKeys(key)
				return asModel(t, updated)
			},
			cursor: func(model Model) int { return model.textState.cursor },
		},
		{
			name: "text timeline",
			setup: func(start int) Model {
				model := NewBuilder().WithPageSize(5).Build()
				model.mode = modeTexts
				model.textState.level = textLevelTimeline
				model.textState.messages = append([]query.MessageSummary(nil), messages...)
				model.textState.cursor = start
				return model
			},
			apply: func(t *testing.T, model Model, key tea.KeyPressMsg) Model {
				updated, _ := model.handleTextTimelineKeys(key)
				return asModel(t, updated)
			},
			cursor: func(model Model) int { return model.textState.cursor },
		},
		{
			name: "meeting list",
			setup: func(start int) Model {
				model := NewBuilder().WithPageSize(5).Build()
				model.mode = modeMeetings
				model.meetingState.messages = append([]query.MessageSummary(nil), messages...)
				model.meetingState.cursor = start
				return model
			},
			apply: func(_ *testing.T, model Model, key tea.KeyPressMsg) Model {
				model.navigateMeetingList(key.String())
				return model
			},
			cursor: func(model Model) int { return model.meetingState.cursor },
		},
		{
			name: "message detail scroll",
			setup: func(start int) Model {
				model := NewBuilder().WithPageSize(5).Build()
				model.level = levelMessageDetail
				model.detailLineCount = 20
				model.detailScroll = start
				return model
			},
			apply: func(t *testing.T, model Model, key tea.KeyPressMsg) Model {
				return applyDetailKey(t, model, key)
			},
			cursor: func(model Model) int { return model.detailScroll },
		},
		{
			name: "meeting detail scroll",
			setup: func(start int) Model {
				model := NewBuilder().WithSize(40, 12).Build()
				model.mode = modeMeetings
				model.meetingState.level = meetingLevelDetail
				model.meetingState.detail = &query.MessageDetail{
					Subject:  "Planning",
					BodyText: strings.Repeat("Transcript line with enough text to wrap.\n", 20),
				}
				model.meetingState.detailScroll = start
				return model
			},
			apply: func(t *testing.T, model Model, key tea.KeyPressMsg) Model {
				updated, _ := model.handleMeetingDetailKeys(key)
				return asModel(t, updated)
			},
			cursor: func(model Model) int { return model.meetingState.detailScroll },
		},
		{
			name: "thread list",
			setup: func(start int) Model {
				model := NewBuilder().WithPageSize(5).Build()
				model.level = levelThreadView
				model.threadMessages = append([]query.MessageSummary(nil), messages...)
				model.threadCursor = start
				return model
			},
			apply: func(t *testing.T, model Model, key tea.KeyPressMsg) Model {
				updated, _ := model.handleThreadViewKeys(key)
				return asModel(t, updated)
			},
			cursor: func(model Model) int { return model.threadCursor },
		},
		{
			name: "account selector",
			setup: func(start int) Model {
				model := NewBuilder().WithAccounts(
					query.AccountInfo{ID: 1, Identifier: "one@example.test"},
					query.AccountInfo{ID: 2, Identifier: "two@example.test"},
				).Build()
				model.modal = modalAccountSelector
				model.modalCursor = start
				return model
			},
			apply: func(t *testing.T, model Model, key tea.KeyPressMsg) Model {
				updated, _ := model.handleAccountSelectorKeys(key)
				return asModel(t, updated)
			},
			cursor: func(model Model) int { return model.modalCursor },
		},
		{
			name: "filter selector",
			setup: func(start int) Model {
				model := NewBuilder().Build()
				model.modal = modalFilterToggle
				model.modalCursor = start
				return model
			},
			apply: func(t *testing.T, model Model, key tea.KeyPressMsg) Model {
				updated, _ := model.handleFilterToggleKeys(key)
				return asModel(t, updated)
			},
			cursor: func(model Model) int { return model.modalCursor },
		},
		{
			name: "attachment selector",
			setup: func(start int) Model {
				model := NewBuilder().WithDetail(&query.MessageDetail{
					ID: 1,
					Attachments: []query.AttachmentInfo{
						{ID: 1, Filename: "one.txt"},
						{ID: 2, Filename: "two.txt"},
					},
				}).Build()
				model.modal = modalExportAttachments
				model.exportCursor = start
				return model
			},
			apply: func(t *testing.T, model Model, key tea.KeyPressMsg) Model {
				updated, _ := model.handleExportAttachmentsKeys(key)
				return asModel(t, updated)
			},
			cursor: func(model Model) int { return model.exportCursor },
		},
		{
			name: "help scroll",
			setup: func(start int) Model {
				model := NewBuilder().WithSize(80, 8).Build()
				model.modal = modalHelp
				model.helpScroll = start
				return model
			},
			apply: func(t *testing.T, model Model, key tea.KeyPressMsg) Model {
				updated, _ := model.handleHelpKeys(key)
				return asModel(t, updated)
			},
			cursor: func(model Model) int { return model.helpScroll },
		},
	}

	directions := []struct {
		name  string
		start int
		key   tea.KeyPressMsg
		want  int
	}{
		{name: "Ctrl+N moves down", start: 0, key: ctrlKey('n'), want: 1},
		{name: "Ctrl+P moves up", start: 1, key: ctrlKey('p'), want: 0},
	}

	for _, surface := range surfaces {
		for _, direction := range directions {
			t.Run(surface.name+"/"+direction.name, func(t *testing.T) {
				model := surface.apply(t, surface.setup(direction.start), direction.key)
				assert.Equal(t, direction.want, surface.cursor(model))
			})
		}
	}
}

func TestFocusedInputKeepsNativeEmacsKeys(t *testing.T) {
	model := NewBuilder().WithRows(
		query.AggregateRow{Key: "one"},
		query.AggregateRow{Key: "two"},
	).Build()
	model.cursor = 1
	model.inlineSearchActive = true
	model.searchInput.SetValue("alice")
	model.searchInput.Focus()

	model.searchInput.CursorEnd()
	updated, _ := model.handleKeyPress(ctrlKey('a'))
	model = asModel(t, updated)
	assert.Equal(t, 0, model.searchInput.Position())
	assert.Equal(t, 1, model.cursor)

	updated, _ = model.handleKeyPress(ctrlKey('e'))
	model = asModel(t, updated)
	assert.Equal(t, len("alice"), model.searchInput.Position())
	assert.Equal(t, 1, model.cursor)

	updated, _ = model.handleKeyPress(ctrlKey('n'))
	model = asModel(t, updated)
	assert.Equal(t, 1, model.cursor, "focused input must not move the aggregate list")

	updated, _ = model.handleKeyPress(ctrlKey('p'))
	model = asModel(t, updated)
	assert.Equal(t, 1, model.cursor, "focused input must not move the aggregate list")
}

func TestHelpAdvertisesEmacsNavigationKeys(t *testing.T) {
	model := NewBuilder().WithSize(100, 50).Build()
	assert.Contains(t, stripANSI(model.renderHelpModal()), "Ctrl+p/n")

	model.mode = modeMeetings
	assert.Contains(t, stripANSI(model.renderHelpModal()), "Ctrl+p/n")
}

func ctrlKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
}
