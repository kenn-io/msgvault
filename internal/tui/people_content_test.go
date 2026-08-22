package tui

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type fakePeopleContentBackend struct {
	*fakePeopleBackend
	meetingPages     map[int64][]*peoplebrowser.MessagePage
	meetingRequests  []peoplebrowser.ContactPageRequest
	filePages        map[int64][]*peoplebrowser.FilePage
	fileRequests     []peoplebrowser.ContactPageRequest
	activityPages    map[int64][]*peoplebrowser.ActivityPage
	activityErrs     []error
	activityRequests []peoplebrowser.ActivityPageRequest
	details          map[int64]*query.MessageDetail
	detailErrs       map[int64][]error
	detailRequests   []int64
}

type fakePeoplePromotionContentBackend struct {
	*fakePeopleContentBackend
	promoted          *store.Person
	promoteRequests   []int64
	attributeRequests []int64
}

func (b *fakePeoplePromotionContentBackend) Promote(
	_ context.Context, participantID int64,
) (*store.Person, error) {
	b.promoteRequests = append(b.promoteRequests, participantID)
	if b.promoted == nil {
		return nil, errors.New("missing promoted person fixture")
	}
	person := *b.promoted
	return &person, nil
}

func (b *fakePeoplePromotionContentBackend) ListAttributes(
	_ context.Context, personID int64,
) (*peoplebrowser.Attributes, error) {
	b.attributeRequests = append(b.attributeRequests, personID)
	return &peoplebrowser.Attributes{PersonID: personID}, nil
}

func (b *fakePeopleContentBackend) ListActivity(
	_ context.Context, request peoplebrowser.ActivityPageRequest,
) (*peoplebrowser.ActivityPage, error) {
	b.activityRequests = append(b.activityRequests, request)
	if len(b.activityErrs) > 0 {
		err := b.activityErrs[0]
		b.activityErrs = b.activityErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	pages := b.activityPages[request.ParticipantID]
	if len(pages) == 0 {
		return &peoplebrowser.ActivityPage{}, nil
	}
	page := pages[0]
	b.activityPages[request.ParticipantID] = pages[1:]
	copy := *page
	copy.Rows = slices.Clone(page.Rows)
	return &copy, nil
}

func (b *fakePeopleContentBackend) ListFiles(
	_ context.Context, request peoplebrowser.ContactPageRequest,
) (*peoplebrowser.FilePage, error) {
	b.fileRequests = append(b.fileRequests, request)
	pages := b.filePages[request.ParticipantID]
	if len(pages) == 0 {
		return &peoplebrowser.FilePage{}, nil
	}
	page := pages[0]
	b.filePages[request.ParticipantID] = pages[1:]
	copy := *page
	copy.Rows = slices.Clone(page.Rows)
	return &copy, nil
}

func (b *fakePeopleContentBackend) ListMeetings(
	_ context.Context, request peoplebrowser.ContactPageRequest,
) (*peoplebrowser.MessagePage, error) {
	b.meetingRequests = append(b.meetingRequests, request)
	pages := b.meetingPages[request.ParticipantID]
	if len(pages) == 0 {
		return &peoplebrowser.MessagePage{}, nil
	}
	page := pages[0]
	b.meetingPages[request.ParticipantID] = pages[1:]
	copy := *page
	copy.Rows = slices.Clone(page.Rows)
	return &copy, nil
}

func (b *fakePeopleContentBackend) GetMessage(
	_ context.Context, messageID int64,
) (*query.MessageDetail, error) {
	b.detailRequests = append(b.detailRequests, messageID)
	if errs := b.detailErrs[messageID]; len(errs) > 0 {
		err := errs[0]
		b.detailErrs[messageID] = errs[1:]
		if err != nil {
			return nil, err
		}
	}
	detail := b.details[messageID]
	if detail == nil {
		return nil, nil
	}
	copy := *detail
	copy.Attachments = slices.Clone(detail.Attachments)
	return &copy, nil
}

func contentTestContact() query.PersonSummary {
	contact := testPerson(7, "Test Contact")
	contact.CacheRevision = "cache-8"
	contact.Cluster = &query.PersonCluster{
		CanonicalID: 7,
		MemberIDs:   []int64{7, 9, 11},
	}
	return contact
}

func peopleContentModel(backend peoplebrowser.Backend, contact *query.PersonSummary) Model {
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabOverview
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = contact
	return model
}

func TestPeopleContactStatusUsesOnlyActiveContentTab(t *testing.T) {
	contact := contentTestContact()
	backend := &fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}
	model := peopleContentModel(backend, &contact)
	model.peopleState.meetingsErr = errors.New("old meetings failure")

	model.peopleState.tab = peopleTabFiles
	model.peopleState.filesErr = errors.New("active files failure")
	view := stripANSI(model.renderPeopleView())
	assert.Contains(t, view, "active files failure")
	assert.NotContains(t, view, "old meetings failure")

	model.peopleState.filesErr = nil
	model.peopleState.filesLoading = true
	view = stripANSI(model.renderPeopleView())
	assert.Contains(t, view, "Loading received files")
	assert.NotContains(t, view, "old meetings failure")

	model.peopleState.filesLoading = false
	model.peopleState.filesErr = errors.New("retry active files")
	model, cmd := sendKey(t, model, key('r'))
	retriedFiles := runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd)
	assert.Equal(t, contact.ID, retriedFiles.participantID)

	model.peopleState.tab = peopleTabActivity
	model.peopleState.activityErr = errors.New("active activity failure")
	view = stripANSI(model.renderPeopleView())
	assert.Contains(t, view, "active activity failure")
	assert.NotContains(t, view, "old meetings failure")

	model.peopleState.activityErr = nil
	model.peopleState.activityLoading = true
	view = stripANSI(model.renderPeopleView())
	assert.Contains(t, view, "Loading contact activity")
	assert.NotContains(t, view, "old meetings failure")
}

func TestPeopleContentDetailErrorsRetryTheSelectedItem(t *testing.T) {
	contact := contentTestContact()

	t.Run("meeting", func(t *testing.T) {
		const messageID = int64(501)
		backend := &fakePeopleContentBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			meetingPages: map[int64][]*peoplebrowser.MessagePage{
				contact.ID: {{Rows: []query.MessageSummary{{ID: messageID, Subject: "Retry meeting"}}}},
			},
			details: map[int64]*query.MessageDetail{
				messageID: {ID: messageID, Subject: "Retry meeting", BodyText: "Recovered transcript"},
			},
			detailErrs: map[int64][]error{messageID: {errors.New("meeting detail failed")}},
		}
		model := peopleContentModel(backend, &contact)
		updated, cmd := model.activatePeopleTab(peopleTabMeetings)
		model = asModel(t, updated)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, cmd))
		model, cmd = sendKey(t, model, keyEnter())
		model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingLoadedMsg](t, cmd))
		assert.Contains(t, stripANSI(model.renderPeopleView()), "meeting detail failed")
		assert.Contains(t, stripANSI(model.renderPeopleView()), "r retry")

		model, cmd = sendKey(t, model, key('r'))
		retried := runPeopleCommandMessage[peopleMeetingLoadedMsg](t, cmd)
		model = sendMsg(t, model, retried)
		assert.Equal(t, peopleLevelMeetingDetail, model.peopleState.level)
		assert.Equal(t, messageID, model.peopleState.selectedContentMessage)
		assert.Contains(t, stripANSI(model.renderPeopleView()), "Recovered transcript")
		assert.Equal(t, []int64{messageID, messageID}, backend.detailRequests)
	})

	t.Run("activity", func(t *testing.T) {
		messageID := int64(502)
		backend := &fakePeopleContentBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			activityPages: map[int64][]*peoplebrowser.ActivityPage{
				contact.ID: {{Rows: []query.EntryRow{{Key: "message:502", AnchorMessageID: &messageID}}}},
			},
			details: map[int64]*query.MessageDetail{
				messageID: {ID: messageID, Subject: "Retry activity", BodyText: "Recovered message"},
			},
			detailErrs: map[int64][]error{messageID: {errors.New("activity detail failed")}},
		}
		model := peopleContentModel(backend, &contact)
		updated, cmd := model.activatePeopleTab(peopleTabActivity)
		model = asModel(t, updated)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd))
		model, cmd = sendKey(t, model, keyEnter())
		model = sendMsg(t, model, runPeopleCommandMessage[peopleActivityMessageLoadedMsg](t, cmd))
		assert.Contains(t, stripANSI(model.renderPeopleView()), "activity detail failed")
		assert.Contains(t, stripANSI(model.renderPeopleView()), "r retry")

		model, cmd = sendKey(t, model, key('r'))
		retried := runPeopleCommandMessage[peopleActivityMessageLoadedMsg](t, cmd)
		model = sendMsg(t, model, retried)
		assert.Equal(t, peopleLevelActivityMessage, model.peopleState.level)
		assert.Equal(t, messageID, model.peopleState.selectedContentMessage)
		assert.Contains(t, stripANSI(model.renderPeopleView()), "Recovered message")
		assert.Equal(t, []int64{messageID, messageID}, backend.detailRequests)
	})

	t.Run("file", func(t *testing.T) {
		const (
			fileID      = int64(80)
			messageID   = int64(503)
			contentHash = "abc123def456abc123def456abc123def456abc123def456abc123def456abc2"
		)
		backend := &fakePeopleContentBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			filePages: map[int64][]*peoplebrowser.FilePage{
				contact.ID: {{Rows: []query.FileRow{{ID: fileID, MessageID: messageID, Filename: "retry.txt"}}}},
			},
			details: map[int64]*query.MessageDetail{
				messageID: {
					ID: messageID, Subject: "Retry file",
					Attachments: []query.AttachmentInfo{{
						ID: fileID, Filename: "retry.txt", ContentHash: contentHash,
					}},
				},
			},
			detailErrs: map[int64][]error{messageID: {errors.New("file detail failed")}},
		}
		model := peopleContentModel(backend, &contact)
		model.actions = NewActionControllerWithOptions(model.engine, ActionControllerOptions{
			DataDir: t.TempDir(),
			AttachmentReader: mapAttachmentReader{data: map[string][]byte{
				contentHash: []byte("attachment bytes"),
			}},
		})
		updated, cmd := model.activatePeopleTab(peopleTabFiles)
		model = asModel(t, updated)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd))
		model, cmd = sendKey(t, model, keyEnter())
		model = sendMsg(t, model, runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, cmd))
		assert.Contains(t, stripANSI(model.renderPeopleView()), "file detail failed")
		assert.Contains(t, stripANSI(model.renderPeopleView()), "r retry")

		model, cmd = sendKey(t, model, key('r'))
		retried := runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, cmd)
		updatedModel, exportCmd := model.Update(retried)
		model = asModel(t, updatedModel)
		require.NotNil(t, exportCmd)
		assert.Equal(t, fileID, model.peopleState.selectedContentFile)
		assert.Equal(t, messageID, model.peopleState.selectedContentMessage)
		assert.Equal(t, []int64{messageID, messageID}, backend.detailRequests)
	})
}

func TestPeopleFileFailureDoesNotFollowCursorToAnotherFile(t *testing.T) {
	contact := contentTestContact()
	const (
		fileA    = int64(80)
		messageA = int64(580)
		fileB    = int64(81)
		messageB = int64(581)
	)
	backend := &fakePeopleContentBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		filePages: map[int64][]*peoplebrowser.FilePage{
			contact.ID: {{Rows: []query.FileRow{
				{ID: fileA, MessageID: messageA, Filename: "failed-a.txt"},
				{ID: fileB, MessageID: messageB, Filename: "current-b.txt"},
			}}},
		},
		detailErrs: map[int64][]error{messageA: {errors.New("file A failed")}},
	}
	model := peopleContentModel(backend, &contact)
	updated, cmd := model.activatePeopleTab(peopleTabFiles)
	model = asModel(t, updated)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd))

	model, cmd = sendKey(t, model, keyEnter())
	model = sendMsg(t, model, runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, cmd))
	assert.Contains(t, stripANSI(model.renderPeopleView()), "file A failed")
	assert.Contains(t, stripANSI(model.renderPeopleView()), "r retry")

	model, _ = sendKey(t, model, keyDown())
	assert.Equal(t, 1, model.peopleState.cursor)
	assert.NoError(t, model.peopleState.filesErr)
	assert.False(t, model.peopleState.fileOpenFailed)
	assert.Zero(t, model.peopleState.selectedContentFile)
	assert.Zero(t, model.peopleState.selectedContentMessage)
	view := stripANSI(model.renderPeopleView())
	assert.NotContains(t, view, "file A failed")
	assert.NotContains(t, view, "r retry")

	model, retry := sendKey(t, model, key('r'))
	assert.Nil(t, retry)
	assert.Equal(t, []int64{messageA}, backend.detailRequests)

	model, cmd = sendKey(t, model, keyEnter())
	opened := runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, cmd)
	assert.Equal(t, fileB, opened.fileID)
	assert.Equal(t, messageB, opened.messageID)
}

func TestPeopleMeetingsUseExactParticipantAndExistingTranscriptReader(t *testing.T) {
	older := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	contact := contentTestContact()
	backend := &fakePeopleContentBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		meetingPages: map[int64][]*peoplebrowser.MessagePage{
			contact.ID: {{
				Rows: []query.MessageSummary{
					{ID: 801, Subject: "Older sync", SentAt: older, MessageType: meetingMessageType},
					{ID: 802, Subject: "Newest sync", SentAt: newer, MessageType: meetingMessageType},
				},
				CacheRevision: "cache-8",
			}},
			99: {{
				Rows: []query.MessageSummary{{
					ID: 899, Subject: "Newest sync", SentAt: newer.Add(time.Hour),
					MessageType: meetingMessageType,
				}},
			}},
		},
		details: map[int64]*query.MessageDetail{
			802: {
				ID: 802, Subject: "Newest sync", SentAt: newer,
				MessageType: meetingMessageType,
				BodyText:    "Opening notes\nneedle in transcript\nClosing notes",
			},
		},
	}
	model := peopleContentModel(backend, &contact)

	updated, cmd := model.activatePeopleTab(peopleTabMeetings)
	model = asModel(t, updated)
	loaded := runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, cmd)
	model = sendMsg(t, model, loaded)

	require.Len(t, backend.meetingRequests, 1)
	assert.Equal(t, peoplebrowser.ContactPageRequest{
		ParticipantID: contact.ID,
		Limit:         peoplePageSize,
	}, backend.meetingRequests[0])
	view := model.renderPeopleView()
	assert.Contains(t, view, "Newest sync")
	assert.Contains(t, view, "Older sync")
	assert.NotContains(t, view, "899")
	assert.Less(t, stringsIndex(view, "Newest sync"), stringsIndex(view, "Older sync"))

	model, cmd = sendKey(t, model, keyEnter())
	detailLoaded := runPeopleCommandMessage[peopleMeetingLoadedMsg](t, cmd)
	model = sendMsg(t, model, detailLoaded)

	assert.Equal(t, []int64{802}, backend.detailRequests)
	assert.Equal(t, peopleLevelMeetingDetail, model.peopleState.level)
	assert.Contains(t, stripANSI(model.renderPeopleView()), "needle in transcript")

	model, _ = sendKey(t, model, key('/'))
	assert.True(t, model.meetingState.detailSearchActive)
	model.meetingState.detailSearchInput.SetValue("needle")
	model, _ = sendKey(t, model, keyEnter())
	assert.Equal(t, "needle", model.meetingState.detailSearchQuery)
	assert.NotEmpty(t, model.meetingState.detailSearchMatches)
}

func TestPeopleFilesPageDeduplicatesAndUsesAttachmentExportPath(t *testing.T) {
	const contentHash = "abc123def456abc123def456abc123def456abc123def456abc123def456abc1"
	newest := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	contact := contentTestContact()
	backend := &fakePeopleContentBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		filePages: map[int64][]*peoplebrowser.FilePage{
			contact.ID: {
				{
					Rows: []query.FileRow{
						{ID: 80, MessageID: 880, OccurredAt: newest, Filename: "notes.pdf", MimeType: "application/pdf", Size: 2048, SourceType: "beeper", SourceIdentifier: "whatsapp"},
						{ID: 81, MessageID: 881, OccurredAt: newest.Add(-time.Hour), Filename: "photo.png", MimeType: "image/png", Size: 512, SourceType: "beeper", SourceIdentifier: "whatsapp"},
					},
					TotalCount: 3, NextCursor: "files-next", CacheRevision: "cache-8",
				},
				{
					Rows: []query.FileRow{
						{ID: 81, MessageID: 881, OccurredAt: newest.Add(-time.Hour), Filename: "photo.png"},
						{ID: 82, MessageID: 882, OccurredAt: newest.Add(-2 * time.Hour), Filename: "archive.zip", MimeType: "application/zip", Size: 4096, SourceType: "email", SourceIdentifier: "account@example.test"},
					},
					TotalCount: 3, CacheRevision: "cache-8",
				},
			},
			99: {{
				Rows: []query.FileRow{{
					ID: 90, MessageID: 890, OccurredAt: newest.Add(time.Hour), Filename: "outbound.txt",
				}},
			}},
		},
		details: map[int64]*query.MessageDetail{
			880: {
				ID: 880, Subject: "Shared notes", SentAt: newest,
				Attachments: []query.AttachmentInfo{{
					ID: 80, Filename: "notes.pdf", MimeType: "application/pdf",
					Size: 2048, ContentHash: contentHash,
				}},
			},
		},
	}
	model := peopleContentModel(backend, &contact)
	model.actions = NewActionControllerWithOptions(model.engine, ActionControllerOptions{
		DataDir: t.TempDir(),
		AttachmentReader: mapAttachmentReader{data: map[string][]byte{
			contentHash: []byte("attachment bytes"),
		}},
	})

	updated, cmd := model.activatePeopleTab(peopleTabFiles)
	model = asModel(t, updated)
	first := runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd)
	model = sendMsg(t, model, first)

	require.Len(t, backend.fileRequests, 1)
	assert.Equal(t, peoplebrowser.ContactPageRequest{
		ParticipantID: contact.ID,
		Limit:         peoplePageSize,
	}, backend.fileRequests[0])
	view := model.renderPeopleView()
	assert.Contains(t, view, "notes.pdf")
	assert.Contains(t, view, "application/pdf")
	assert.Contains(t, view, "2.0 KB")
	assert.Contains(t, view, "Beeper/WhatsApp")
	assert.NotContains(t, view, "outbound.txt")

	model, cmd = sendKey(t, model, keyDown())
	second := runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd)
	model = sendMsg(t, model, second)

	require.Len(t, backend.fileRequests, 2)
	assert.Equal(t, "files-next", backend.fileRequests[1].Cursor)
	require.Len(t, model.peopleState.files, 3)
	assert.Equal(t, []int64{80, 81, 82}, []int64{
		model.peopleState.files[0].ID,
		model.peopleState.files[1].ID,
		model.peopleState.files[2].ID,
	})

	model, _ = sendKey(t, model, keyHome())
	model, cmd = sendKey(t, model, keyEnter())
	detailLoaded := runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, cmd)
	updatedModel, exportCmd := model.Update(detailLoaded)
	model = asModel(t, updatedModel)
	require.NotNil(t, exportCmd)

	outputDir := t.TempDir()
	t.Chdir(outputDir)
	exportMessage, ok := exportCmd().(peopleFileExportedMsg)
	require.True(t, ok)
	require.NoError(t, exportMessage.result.Err)
	model = sendMsg(t, model, exportMessage)
	assert.Equal(t, modalExportResult, model.modal)
	assert.False(t, model.loading)
	zipped, err := zip.OpenReader(filepath.Join(outputDir, "Shared notes_880.zip"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, zipped.Close()) })
	require.Len(t, zipped.File, 1)
	assert.Equal(t, "notes.pdf", zipped.File[0].Name)
}

func TestPeopleFileExportCompletionRequiresCurrentPeopleSelection(t *testing.T) {
	contact := contentTestContact()
	base := peopleContentModel(
		&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact,
	)
	base.peopleState.tab = peopleTabFiles
	base.peopleState.files = []query.FileRow{
		{ID: 80, MessageID: 880, Filename: "first.txt"},
		{ID: 81, MessageID: 881, Filename: "second.txt"},
	}
	base.peopleState.filesLoaded = true
	base.peopleState.selectedContentFile = 80
	base.peopleState.selectedContentMessage = 880
	base.peopleState.fileOpening = true
	base.loading = true
	result := peopleFileExportedMsg{
		result:    ExportResultMsg{Result: "Exported first.txt"},
		requestID: base.peopleState.requestID, participantID: contact.ID,
		tab: peopleTabFiles, fileID: 80, messageID: 880,
		presentationGeneration: base.presentationGeneration,
	}

	t.Run("valid", func(t *testing.T) {
		updated := sendMsg(t, base, result)
		assert.Equal(t, modalExportResult, updated.modal)
		assert.Equal(t, "Exported first.txt", updated.modalResult)
		assert.False(t, updated.peopleState.fileOpening)
	})

	t.Run("mode changed", func(t *testing.T) {
		changed, _, handled := base.handleGlobalKeys(key('m'))
		require.True(t, handled)
		updated := sendMsg(t, changed, result)
		assert.Equal(t, modalNone, updated.modal)
	})

	t.Run("tab changed", func(t *testing.T) {
		changedModel, _ := base.activatePeopleTab(peopleTabActivity)
		changed := asModel(t, changedModel)
		updated := sendMsg(t, changed, result)
		assert.Equal(t, modalNone, updated.modal)
	})

	t.Run("contact changed", func(t *testing.T) {
		changed := base
		other := testPerson(9, "Other Contact")
		changed.peopleState.participantID = other.ID
		changed.peopleState.contact = &other
		updated := sendMsg(t, changed, result)
		assert.Equal(t, modalNone, updated.modal)
	})

	t.Run("selection changed", func(t *testing.T) {
		changed, _ := sendKey(t, base, keyDown())
		assert.False(t, changed.peopleState.fileOpening)
		updated := sendMsg(t, changed, result)
		assert.Equal(t, modalNone, updated.modal)
	})

	t.Run("valid error", func(t *testing.T) {
		failed := result
		failed.result = ExportResultMsg{Err: errors.New("disk full")}
		updated := sendMsg(t, base, failed)
		assert.Equal(t, modalExportResult, updated.modal)
		assert.Contains(t, updated.modalResult, "disk full")
	})
}

func TestPeoplePromotionSettlesPendingFileExport(t *testing.T) {
	const (
		fileID      = int64(80)
		messageID   = int64(880)
		contentHash = "abc123def456abc123def456abc123def456abc123def456abc123def456abc3"
	)
	contact := contentTestContact()
	backend := &fakePeoplePromotionContentBackend{
		fakePeopleContentBackend: &fakePeopleContentBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			filePages: map[int64][]*peoplebrowser.FilePage{
				contact.ID: {{Rows: []query.FileRow{{
					ID: fileID, MessageID: messageID, Filename: "promotion.txt",
				}}}},
			},
			details: map[int64]*query.MessageDetail{
				messageID: {
					ID: messageID, Subject: "Promotion export",
					Attachments: []query.AttachmentInfo{{
						ID: fileID, Filename: "promotion.txt", ContentHash: contentHash,
					}},
				},
			},
		},
		promoted: &store.Person{ID: 51, Revision: 1, ParticipantIDs: []int64{contact.ID}},
	}
	model := peopleContentModel(backend, &contact)
	model.actions = NewActionControllerWithOptions(model.engine, ActionControllerOptions{
		DataDir: t.TempDir(),
		AttachmentReader: mapAttachmentReader{data: map[string][]byte{
			contentHash: []byte("attachment bytes"),
		}},
	})
	updated, cmd := model.activatePeopleTab(peopleTabFiles)
	model = asModel(t, updated)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd))
	model, cmd = sendKey(t, model, keyEnter())
	fileLoaded := runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, cmd)
	updatedModel, exportCmd := model.Update(fileLoaded)
	model = asModel(t, updatedModel)
	require.NotNil(t, exportCmd)
	assert.True(t, model.peopleState.fileOpening)

	model, promoteCmd := sendKey(t, model, key('p'))
	require.NotNil(t, promoteCmd)
	assert.False(t, model.peopleState.fileOpening)
	assert.False(t, model.peopleState.fileOpenFailed)
	assert.Zero(t, model.peopleState.selectedContentFile)
	assert.Zero(t, model.peopleState.selectedContentMessage)
	assert.True(t, model.peopleState.promoting)
	require.Len(t, model.peopleState.files, 1)

	outputDir := t.TempDir()
	t.Chdir(outputDir)
	staleExport, ok := exportCmd().(peopleFileExportedMsg)
	require.True(t, ok)
	model = sendMsg(t, model, staleExport)
	assert.Equal(t, modalNone, model.modal)
	assert.False(t, model.peopleState.fileOpening)

	promoted := runPeopleCommandMessage[peoplePromotedMsg](t, promoteCmd)
	updatedModel, attributesCmd := model.Update(promoted)
	model = asModel(t, updatedModel)
	require.NotNil(t, attributesCmd)
	require.NotNil(t, model.peopleState.contact.Profile)
	assert.Equal(t, int64(51), model.peopleState.contact.Profile.ID)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleAttributesLoadedMsg](t, attributesCmd))

	assert.Equal(t, peopleTabFiles, model.peopleState.tab)
	assert.False(t, model.loading)
	assert.False(t, model.peopleState.fileOpening)
	require.Len(t, model.peopleState.files, 1)
	assert.Contains(t, stripANSI(model.renderPeopleView()), "promotion.txt")
	assert.NotContains(t, stripANSI(model.renderPeopleView()), "Loading received files")
	assert.Equal(t, []int64{contact.ID}, backend.promoteRequests)
	assert.Equal(t, []int64{51}, backend.attributeRequests)

	model, reopen := sendKey(t, model, keyEnter())
	reopened := runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, reopen)
	assert.Equal(t, messageID, reopened.messageID)
	assert.Equal(t, []int64{messageID, messageID}, backend.detailRequests)
}

func TestPeopleMeetingDetailPreservesStandaloneMeetingReader(t *testing.T) {
	contact := contentTestContact()
	const (
		standaloneID = int64(601)
		contactID    = int64(602)
	)
	backend := &fakePeopleContentBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		meetingPages: map[int64][]*peoplebrowser.MessagePage{
			contact.ID: {{Rows: []query.MessageSummary{{ID: contactID, Subject: "Contact meeting"}}}},
		},
		details: map[int64]*query.MessageDetail{
			contactID: {ID: contactID, Subject: "Contact meeting", BodyText: "Contact transcript"},
		},
	}
	model := peopleContentModel(backend, &contact)
	standaloneDetail := &query.MessageDetail{
		ID: standaloneID, Subject: "Standalone meeting", BodyText: "Standalone transcript",
	}
	model.mode = modeMeetings
	model.meetingState.initialized = true
	model.meetingState.level = meetingLevelDetail
	model.meetingState.messages = []query.MessageSummary{{ID: standaloneID, Subject: "Standalone meeting"}}
	model.meetingState.detail = standaloneDetail
	model.meetingState.detailScroll = 3
	model.meetingState.detailSearchQuery = "standalone needle"
	model.meetingState.detailSearchInput.SetValue("standalone needle")
	model.meetingState.detailSearchMatches = []int{4}
	model.meetingState.detailSearchMatchIndex = 0

	entered, _, handled := model.handleGlobalKeys(key('m'))
	require.True(t, handled)
	model = entered
	require.Equal(t, modePeople, model.mode)

	updated, cmd := model.activatePeopleTab(peopleTabMeetings)
	model = asModel(t, updated)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, cmd))
	model, cmd = sendKey(t, model, keyEnter())
	model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingLoadedMsg](t, cmd))
	require.NotNil(t, model.meetingState.detail)
	assert.Equal(t, contactID, model.meetingState.detail.ID)
	model, _ = sendKey(t, model, key('/'))
	model.meetingState.detailSearchInput.SetValue("contact needle")
	model, _ = sendKey(t, model, keyEnter())
	assert.Equal(t, "contact needle", model.meetingState.detailSearchQuery)

	left, _, handled := model.handleGlobalKeys(key('m'))
	require.True(t, handled)
	model = left
	require.Equal(t, modeEmail, model.mode)
	model.mode = modeTexts
	returned, _, handled := model.handleGlobalKeys(key('m'))
	require.True(t, handled)
	model = returned
	require.Equal(t, modeMeetings, model.mode)

	assert.Equal(t, meetingLevelDetail, model.meetingState.level)
	assert.Same(t, standaloneDetail, model.meetingState.detail)
	assert.Equal(t, 3, model.meetingState.detailScroll)
	assert.Equal(t, "standalone needle", model.meetingState.detailSearchQuery)
	assert.Equal(t, "standalone needle", model.meetingState.detailSearchInput.Value())
	assert.Equal(t, []int{4}, model.meetingState.detailSearchMatches)
	assert.Contains(t, stripANSI(model.renderMeetingView()), "Standalone transcript")

	reentered, _, handled := model.handleGlobalKeys(key('m'))
	require.True(t, handled)
	model = reentered
	require.Equal(t, modePeople, model.mode)
	require.NotNil(t, model.meetingState.detail)
	assert.Equal(t, contactID, model.meetingState.detail.ID)
	model, _ = sendKey(t, model, keyEsc())
	assert.Equal(t, peopleLevelContact, model.peopleState.level)
	assert.Nil(t, model.meetingState.detail)

	left, _, handled = model.handleGlobalKeys(key('m'))
	require.True(t, handled)
	model = left
	model.mode = modeTexts
	returned, _, handled = model.handleGlobalKeys(key('m'))
	require.True(t, handled)
	model = returned
	assert.Same(t, standaloneDetail, model.meetingState.detail)
	assert.Equal(t, "standalone needle", model.meetingState.detailSearchQuery)
}

func TestPeopleContentTabsAdvertiseImplementedNavigation(t *testing.T) {
	contact := contentTestContact()
	for _, tab := range []peopleTab{peopleTabMeetings, peopleTabFiles, peopleTabActivity} {
		model := peopleContentModel(
			&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact,
		)
		model.peopleState.tab = tab
		switch tab {
		case peopleTabMeetings:
			model.peopleState.meetingsErr = errors.New("retry meetings")
		case peopleTabFiles:
			model.peopleState.filesErr = errors.New("retry files")
		case peopleTabActivity:
			model.peopleState.activityErr = errors.New("retry activity")
		}

		wide := stripANSI(model.peopleFooterView())
		assert.Contains(t, wide, "↑/↓")
		assert.Contains(t, wide, "PgUp/PgDn")
		assert.Contains(t, wide, "Enter")
		assert.Contains(t, wide, "r retry")
		assert.Contains(t, wide, "Tab")

		model.width = 44
		narrow := stripANSI(model.peopleFooterView())
		assert.Contains(t, narrow, "↑/↓")
		assert.Contains(t, narrow, "Pg")
		assert.Contains(t, narrow, "Enter")
		assert.Contains(t, narrow, "r")
		assert.Contains(t, narrow, "Tab")
	}
}

func TestPeopleActivityGroupsLocalDaysPagesAndOpensExistingMessageReader(t *testing.T) {
	local := time.FixedZone("Test Local", 2*60*60)
	contact := contentTestContact()
	chatMessageID := int64(902)
	emailMessageID := int64(901)
	backend := &fakePeopleContentBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		activityPages: map[int64][]*peoplebrowser.ActivityPage{
			contact.ID: {
				{
					Rows: []query.EntryRow{
						{
							Key: "email:901", Kind: query.EntryEmail, AnchorMessageID: &emailMessageID,
							OccurredAt: time.Date(2026, 8, 20, 16, 42, 0, 0, time.UTC),
							SourceType: "gmail", MessageType: "email", Title: "Re: Plans",
							MatchedRecipientIdentities: []string{"contact@example.test"},
						},
						{
							Key: "message:902", Kind: query.EntryConversation, AnchorMessageID: &chatMessageID,
							OccurredAt: time.Date(2026, 8, 20, 23, 30, 0, 0, time.UTC),
							SourceType: "beeper", SourceIdentifier: "whatsapp",
							MessageType: "whatsapp", Title: "Morning",
							MatchedSenderIdentities: []string{"contact@example.test"},
						},
						{
							Key: "meeting:903", Kind: query.EntryMeeting,
							OccurredAt:  time.Date(2026, 8, 20, 22, 0, 0, 0, time.UTC),
							SourceType:  "meeting_import",
							MessageType: meetingMessageType,
							Title:       "Weekly sync",
						},
					},
					TotalCount: 4, NextCursor: "activity-next", CacheRevision: "cache-8",
				},
				{
					Rows: []query.EntryRow{
						{Key: "email:901", Kind: query.EntryEmail, OccurredAt: time.Date(2026, 8, 20, 16, 42, 0, 0, time.UTC), Title: "Re: Plans"},
						{Key: "email:900", Kind: query.EntryEmail, OccurredAt: time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC), SourceType: "gmail", MessageType: "email", Title: "Earlier note"},
					},
					TotalCount: 4, CacheRevision: "cache-8",
				},
			},
		},
		details: map[int64]*query.MessageDetail{
			chatMessageID: {
				ID: chatMessageID, Subject: "Morning", SentAt: time.Date(2026, 8, 20, 23, 30, 0, 0, time.UTC),
				MessageType: "whatsapp", BodyText: "Existing activity detail body",
			},
		},
	}
	model := peopleContentModel(backend, &contact)
	model.peopleState.location = local

	updated, cmd := model.activatePeopleTab(peopleTabActivity)
	model = asModel(t, updated)
	first := runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd)
	model = sendMsg(t, model, first)

	require.Len(t, backend.activityRequests, 1)
	assert.Equal(t, peoplebrowser.ActivityPageRequest{
		ParticipantID: contact.ID,
		Limit:         peoplePageSize,
	}, backend.activityRequests[0])
	view := model.renderPeopleView()
	assert.Contains(t, view, "2026-08-21")
	assert.Contains(t, view, "2026-08-20")
	assert.Contains(t, view, "● 01:30")
	assert.Contains(t, view, "▶ ● 01:30")
	assert.Contains(t, view, "│")
	assert.Contains(t, view, "WhatsApp")
	assert.Contains(t, view, "received")
	assert.Contains(t, view, "sent")
	assert.Contains(t, view, "Meeting")
	assert.Less(t, stringsIndex(view, "Morning"), stringsIndex(view, "Re: Plans"))

	model.width = 44
	narrow := model.renderPeopleView()
	assert.Contains(t, narrow, "2026-08-21")
	assert.Contains(t, narrow, "● 01:30")
	assert.Contains(t, narrow, "Morning")
	assert.NotContains(t, narrow, "WhatsApp")
	assert.NotContains(t, narrow, "received")

	model.width = 100
	model, cmd = sendKey(t, model, key('G'))
	second := runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd)
	model = sendMsg(t, model, second)
	require.Len(t, model.peopleState.activity, 4)
	assert.Equal(t, []string{"message:902", "meeting:903", "email:901", "email:900"}, []string{
		model.peopleState.activity[0].Key,
		model.peopleState.activity[1].Key,
		model.peopleState.activity[2].Key,
		model.peopleState.activity[3].Key,
	})

	model, _ = sendKey(t, model, keyHome())
	model, cmd = sendKey(t, model, keyEnter())
	detailLoaded := runPeopleCommandMessage[peopleActivityMessageLoadedMsg](t, cmd)
	model = sendMsg(t, model, detailLoaded)
	assert.Equal(t, []int64{chatMessageID}, backend.detailRequests)
	assert.Equal(t, peopleLevelActivityMessage, model.peopleState.level)
	assert.Contains(t, model.renderPeopleView(), "Existing activity detail body")
}

func TestPeopleActivityErrorRetriesToEmptyState(t *testing.T) {
	contact := contentTestContact()
	backend := &fakePeopleContentBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		activityErrs:      []error{errors.New("temporary activity failure")},
		activityPages: map[int64][]*peoplebrowser.ActivityPage{
			contact.ID: {{CacheRevision: "cache-8"}},
		},
	}
	model := peopleContentModel(backend, &contact)

	updated, cmd := model.activatePeopleTab(peopleTabActivity)
	model = asModel(t, updated)
	failed := runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd)
	model = sendMsg(t, model, failed)
	view := model.renderPeopleView()
	assert.Contains(t, view, "temporary activity failure")
	assert.Contains(t, view, "r retry")

	model, cmd = sendKey(t, model, key('r'))
	retried := runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd)
	model = sendMsg(t, model, retried)
	assert.Contains(t, model.renderPeopleView(), "No activity found for this contact")
}

func TestPeopleContentPaginationRevisionRestartsOnceThenPauses(t *testing.T) {
	contact := contentTestContact()
	meetingRow := func(id int64, at time.Time) query.MessageSummary {
		return query.MessageSummary{ID: id, Subject: "Meeting", SentAt: at, MessageType: meetingMessageType}
	}
	fileRow := func(id int64, at time.Time) query.FileRow {
		return query.FileRow{ID: id, MessageID: id + 1000, OccurredAt: at, Filename: "file.txt"}
	}
	activityRow := func(id int64, at time.Time) query.EntryRow {
		return query.EntryRow{Key: fmt.Sprintf("entry:%d", id), OccurredAt: at, Title: "Activity"}
	}
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	t.Run("meetings", func(t *testing.T) {
		backend := &fakePeopleContentBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			meetingPages: map[int64][]*peoplebrowser.MessagePage{
				contact.ID: {
					{Rows: []query.MessageSummary{meetingRow(1, now), meetingRow(2, now.Add(-time.Minute))}, NextCursor: "meeting-a", CacheRevision: "rev-1"},
					{Rows: []query.MessageSummary{meetingRow(3, now.Add(-2*time.Minute))}, CacheRevision: "rev-2"},
					{Rows: []query.MessageSummary{meetingRow(4, now)}, NextCursor: "meeting-b", CacheRevision: "rev-3"},
					{Rows: []query.MessageSummary{meetingRow(5, now.Add(-time.Minute))}, CacheRevision: "rev-4"},
				},
			},
		}
		model := peopleContentModel(backend, &contact)
		updated, cmd := model.activatePeopleTab(peopleTabMeetings)
		model = asModel(t, updated)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, cmd))

		model, cmd = sendKey(t, model, keyDown())
		mismatch := runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, cmd)
		updatedModel, restartCmd := model.Update(mismatch)
		model = asModel(t, updatedModel)
		require.NotNil(t, restartCmd)
		assert.True(t, model.peopleState.meetingsRestarted)
		assert.Empty(t, model.peopleState.meetings)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, restartCmd))

		model, cmd = sendKey(t, model, keyDown())
		secondMismatch := runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, cmd)
		updatedModel, retryCmd := model.Update(secondMismatch)
		model = asModel(t, updatedModel)
		assert.Nil(t, retryCmd)
		assert.ErrorIs(t, model.peopleState.meetingsErr, errPeopleContentChanged)
		assert.Contains(t, model.renderPeopleView(), "r retry")
		assert.Nil(t, model.maybeLoadMorePeopleMeetings())
		assert.Equal(t, []string{"", "meeting-a", "", "meeting-b"}, contentRequestCursors(backend.meetingRequests))
	})

	t.Run("files", func(t *testing.T) {
		backend := &fakePeopleContentBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			filePages: map[int64][]*peoplebrowser.FilePage{
				contact.ID: {
					{Rows: []query.FileRow{fileRow(1, now), fileRow(2, now.Add(-time.Minute))}, NextCursor: "file-a", CacheRevision: "rev-1"},
					{Rows: []query.FileRow{fileRow(3, now.Add(-2*time.Minute))}, CacheRevision: "rev-2"},
					{Rows: []query.FileRow{fileRow(4, now)}, NextCursor: "file-b", CacheRevision: "rev-3"},
					{Rows: []query.FileRow{fileRow(5, now.Add(-time.Minute))}, CacheRevision: "rev-4"},
				},
			},
		}
		model := peopleContentModel(backend, &contact)
		updated, cmd := model.activatePeopleTab(peopleTabFiles)
		model = asModel(t, updated)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd))

		model, cmd = sendKey(t, model, keyDown())
		mismatch := runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd)
		updatedModel, restartCmd := model.Update(mismatch)
		model = asModel(t, updatedModel)
		require.NotNil(t, restartCmd)
		assert.True(t, model.peopleState.filesRestarted)
		assert.Empty(t, model.peopleState.files)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleFilesLoadedMsg](t, restartCmd))

		model, cmd = sendKey(t, model, keyDown())
		secondMismatch := runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd)
		updatedModel, retryCmd := model.Update(secondMismatch)
		model = asModel(t, updatedModel)
		assert.Nil(t, retryCmd)
		assert.ErrorIs(t, model.peopleState.filesErr, errPeopleContentChanged)
		assert.Contains(t, model.renderPeopleView(), "r retry")
		assert.Nil(t, model.maybeLoadMorePeopleFiles())
		assert.Equal(t, []string{"", "file-a", "", "file-b"}, contentRequestCursors(backend.fileRequests))
	})

	t.Run("activity", func(t *testing.T) {
		backend := &fakePeopleContentBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			activityPages: map[int64][]*peoplebrowser.ActivityPage{
				contact.ID: {
					{Rows: []query.EntryRow{activityRow(1, now), activityRow(2, now.Add(-time.Minute))}, NextCursor: "activity-a", CacheRevision: "rev-1"},
					{Rows: []query.EntryRow{activityRow(3, now.Add(-2*time.Minute))}, CacheRevision: "rev-2"},
					{Rows: []query.EntryRow{activityRow(4, now)}, NextCursor: "activity-b", CacheRevision: "rev-3"},
					{Rows: []query.EntryRow{activityRow(5, now.Add(-time.Minute))}, CacheRevision: "rev-4"},
				},
			},
		}
		model := peopleContentModel(backend, &contact)
		updated, cmd := model.activatePeopleTab(peopleTabActivity)
		model = asModel(t, updated)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd))

		model, cmd = sendKey(t, model, keyDown())
		mismatch := runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd)
		updatedModel, restartCmd := model.Update(mismatch)
		model = asModel(t, updatedModel)
		require.NotNil(t, restartCmd)
		assert.True(t, model.peopleState.activityRestarted)
		assert.Empty(t, model.peopleState.activity)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleActivityLoadedMsg](t, restartCmd))

		model, cmd = sendKey(t, model, keyDown())
		secondMismatch := runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd)
		updatedModel, retryCmd := model.Update(secondMismatch)
		model = asModel(t, updatedModel)
		assert.Nil(t, retryCmd)
		assert.ErrorIs(t, model.peopleState.activityErr, errPeopleContentChanged)
		assert.Contains(t, model.renderPeopleView(), "r retry")
		assert.Nil(t, model.maybeLoadMorePeopleActivity())
		assert.Equal(t, []string{"", "activity-a", "", "activity-b"}, contentRequestCursors(backend.activityRequests))
	})
}

func contentRequestCursors(requests []peoplebrowser.ContactPageRequest) []string {
	cursors := make([]string, len(requests))
	for i, request := range requests {
		cursors[i] = request.Cursor
	}
	return cursors
}

func TestPeopleContentTabLeaveAbandonsPaginationAndReloads(t *testing.T) {
	contact := contentTestContact()
	loadedAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	backend := &fakePeopleContentBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		filePages: map[int64][]*peoplebrowser.FilePage{
			contact.ID: {
				{Rows: []query.FileRow{{ID: 2, MessageID: 1002, OccurredAt: loadedAt.Add(-time.Minute)}}, CacheRevision: "cache-8"},
				{Rows: []query.FileRow{{ID: 3, MessageID: 1003, OccurredAt: loadedAt}}, CacheRevision: "cache-9"},
			},
		},
	}
	model := peopleContentModel(backend, &contact)
	model.peopleState.tab = peopleTabFiles
	model.peopleState.files = []query.FileRow{{
		ID: 1, MessageID: 1001, OccurredAt: loadedAt,
	}}
	model.peopleState.filesLoaded = true
	model.peopleState.filesNextCursor = "old-next"
	model.peopleState.filesCacheRevision = "cache-8"

	model, pendingFileCmd := sendKey(t, model, keyDown())
	require.NotNil(t, pendingFileCmd)
	assert.True(t, model.peopleState.filesLoadingMore)

	updated, pendingActivityCmd := model.activatePeopleTab(peopleTabActivity)
	model = asModel(t, updated)
	require.NotNil(t, pendingActivityCmd)
	assert.False(t, model.peopleState.filesLoading)
	assert.False(t, model.peopleState.filesLoadingMore)
	assert.False(t, model.peopleState.filesLoaded)

	staleFile := runPeopleCommandMessage[peopleFilesLoadedMsg](t, pendingFileCmd)
	model = sendMsg(t, model, staleFile)
	assert.Equal(t, peopleTabActivity, model.peopleState.tab)
	assert.Empty(t, model.peopleState.files)

	updated, reloadCmd := model.activatePeopleTab(peopleTabFiles)
	model = asModel(t, updated)
	require.NotNil(t, reloadCmd)
	reloaded := runPeopleCommandMessage[peopleFilesLoadedMsg](t, reloadCmd)
	model = sendMsg(t, model, reloaded)
	require.Len(t, model.peopleState.files, 1)
	assert.Equal(t, int64(3), model.peopleState.files[0].ID)
	assert.Equal(t, []string{"old-next", ""}, contentRequestCursors(backend.fileRequests))

	staleActivity := runPeopleCommandMessage[peopleActivityLoadedMsg](t, pendingActivityCmd)
	model = sendMsg(t, model, staleActivity)
	assert.Equal(t, peopleTabFiles, model.peopleState.tab)
	assert.Empty(t, model.peopleState.activity)
}

func TestPeopleContentPageResponseRejectsWrongCursorContext(t *testing.T) {
	contact := contentTestContact()
	model := peopleContentModel(&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact)
	model.peopleState.tab = peopleTabFiles
	model.peopleState.files = []query.FileRow{{ID: 1}}
	model.peopleState.filesLoaded = true
	model.peopleState.filesLoading = true
	model.peopleState.filesLoadingMore = true
	model.peopleState.filesNextCursor = "expected-cursor"
	model.peopleState.filesCacheRevision = "cache-8"

	stale := peopleFilesLoadedMsg{
		page: &peoplebrowser.FilePage{
			Rows: []query.FileRow{{ID: 2}}, CacheRevision: "cache-8",
		},
		requestID: model.peopleState.requestID, participantID: contact.ID,
		cursor: "wrong-cursor", append: true,
		presentationGeneration: model.presentationGeneration,
	}
	model = sendMsg(t, model, stale)

	assert.Equal(t, []query.FileRow{{ID: 1}}, model.peopleState.files)
	assert.True(t, model.peopleState.filesLoading)
	assert.True(t, model.peopleState.filesLoadingMore)
}

func TestPeopleContentModeLeaveReloadsAbandonedTab(t *testing.T) {
	contact := contentTestContact()
	backend := &fakePeopleContentBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		meetingPages: map[int64][]*peoplebrowser.MessagePage{
			contact.ID: {
				{Rows: []query.MessageSummary{{ID: 1, Subject: "Stale meeting"}}, CacheRevision: "cache-8"},
				{Rows: []query.MessageSummary{{ID: 2, Subject: "Fresh meeting"}}, CacheRevision: "cache-9"},
			},
		},
	}
	model := peopleContentModel(backend, &contact)

	updated, pending := model.activatePeopleTab(peopleTabMeetings)
	model = asModel(t, updated)
	require.NotNil(t, pending)
	model, fresh := leaveAndReenterPeople(t, model)

	require.NotNil(t, fresh)
	assert.True(t, model.peopleState.meetingsLoading)
	stale := runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, pending)
	model = sendMsg(t, model, stale)
	assert.Empty(t, model.peopleState.meetings)
	loaded := runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, fresh)
	model = sendMsg(t, model, loaded)
	require.Len(t, model.peopleState.meetings, 1)
	assert.Equal(t, "Fresh meeting", model.peopleState.meetings[0].Subject)
	assert.Equal(t, []string{"", ""}, contentRequestCursors(backend.meetingRequests))
}

func TestPeopleContentNilAppendPageBecomesRetryableError(t *testing.T) {
	contact := contentTestContact()

	t.Run("meetings", func(t *testing.T) {
		model := peopleContentModel(&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact)
		model.peopleState.tab = peopleTabMeetings
		model.peopleState.meetingsNextCursor = "next"
		model.peopleState.meetingsCacheRevision = "cache-8"
		message := peopleMeetingsLoadedMsg{
			requestID: model.peopleState.requestID, participantID: contact.ID,
			cursor: "next", append: true, presentationGeneration: model.presentationGeneration,
		}
		assert.NotPanics(t, func() {
			model = sendMsg(t, model, message)
		})
		assert.ErrorContains(t, model.peopleState.meetingsErr, "empty response")
	})

	t.Run("files", func(t *testing.T) {
		model := peopleContentModel(&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact)
		model.peopleState.tab = peopleTabFiles
		model.peopleState.filesNextCursor = "next"
		model.peopleState.filesCacheRevision = "cache-8"
		message := peopleFilesLoadedMsg{
			requestID: model.peopleState.requestID, participantID: contact.ID,
			cursor: "next", append: true, presentationGeneration: model.presentationGeneration,
		}
		assert.NotPanics(t, func() {
			model = sendMsg(t, model, message)
		})
		assert.ErrorContains(t, model.peopleState.filesErr, "empty response")
	})

	t.Run("activity", func(t *testing.T) {
		model := peopleContentModel(&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact)
		model.peopleState.tab = peopleTabActivity
		model.peopleState.activityNextCursor = "next"
		model.peopleState.activityCacheRevision = "cache-8"
		message := peopleActivityLoadedMsg{
			requestID: model.peopleState.requestID, participantID: contact.ID,
			cursor: "next", append: true, presentationGeneration: model.presentationGeneration,
		}
		assert.NotPanics(t, func() {
			model = sendMsg(t, model, message)
		})
		assert.ErrorContains(t, model.peopleState.activityErr, "empty response")
	})
}

func TestPeopleContentModeReentryReloadsAbandonedDetail(t *testing.T) {
	contact := contentTestContact()
	messageID := int64(77)

	t.Run("meeting transcript", func(t *testing.T) {
		backend := &fakePeopleContentBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			meetingPages: map[int64][]*peoplebrowser.MessagePage{
				contact.ID: {{Rows: []query.MessageSummary{{ID: messageID, Subject: "Meeting"}}, CacheRevision: "cache-8"}},
			},
			details: map[int64]*query.MessageDetail{
				messageID: {ID: messageID, Subject: "Meeting", BodyText: "Fresh transcript"},
			},
		}
		model := peopleContentModel(backend, &contact)
		updated, cmd := model.activatePeopleTab(peopleTabMeetings)
		model = asModel(t, updated)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, cmd))
		model, pending := sendKey(t, model, keyEnter())

		model, fresh := leaveAndReenterPeople(t, model)
		require.NotNil(t, fresh)
		assert.True(t, model.meetingState.detailLoading)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingLoadedMsg](t, pending))
		assert.Nil(t, model.meetingState.detail)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingLoadedMsg](t, fresh))
		require.NotNil(t, model.meetingState.detail)
		assert.Equal(t, "Fresh transcript", model.meetingState.detail.BodyText)
		assert.Equal(t, []int64{messageID, messageID}, backend.detailRequests)
	})

	t.Run("activity message", func(t *testing.T) {
		backend := &fakePeopleContentBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			activityPages: map[int64][]*peoplebrowser.ActivityPage{
				contact.ID: {{Rows: []query.EntryRow{{Key: "message:77", AnchorMessageID: &messageID}}, CacheRevision: "cache-8"}},
			},
			details: map[int64]*query.MessageDetail{
				messageID: {ID: messageID, Subject: "Activity", BodyText: "Fresh activity body"},
			},
		}
		model := peopleContentModel(backend, &contact)
		updated, cmd := model.activatePeopleTab(peopleTabActivity)
		model = asModel(t, updated)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd))
		model, pending := sendKey(t, model, keyEnter())

		model, fresh := leaveAndReenterPeople(t, model)
		require.NotNil(t, fresh)
		assert.True(t, model.peopleState.messageLoading)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleActivityMessageLoadedMsg](t, pending))
		assert.Nil(t, model.messageDetail)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleActivityMessageLoadedMsg](t, fresh))
		require.NotNil(t, model.messageDetail)
		assert.Equal(t, "Fresh activity body", model.messageDetail.BodyText)
		assert.Equal(t, []int64{messageID, messageID}, backend.detailRequests)
	})
}

func TestPeopleActivitySelectionRemainsVisibleAtConstrainedHeight(t *testing.T) {
	contact := contentTestContact()
	model := peopleContentModel(&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact)
	model.pageSize = 7
	model.peopleState.tab = peopleTabActivity
	for i := range 8 {
		model.peopleState.activity = append(model.peopleState.activity, query.EntryRow{
			Key:        fmt.Sprintf("entry:%d", i),
			OccurredAt: time.Date(2026, 8, 20, 12-i, 0, 0, 0, time.UTC),
			Title:      fmt.Sprintf("Activity %d", i),
		})
	}
	model.peopleState.activityLoaded = true

	model, _ = sendKey(t, model, key('G'))

	assert.Equal(t, 7, model.peopleState.cursor)
	view := stripANSI(model.renderPeopleView())
	assert.Contains(t, view, "▶ ● 05:00")
	assert.Contains(t, view, "Activity 7")
}

func stringsIndex(haystack, needle string) int {
	return strings.Index(haystack, needle)
}
