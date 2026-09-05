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

var errFakePeopleContentMessageNotFound = errors.New("fake People content message not found")

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
	copied := *page
	copied.Rows = slices.Clone(page.Rows)
	return &copied, nil
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
	copied := *page
	copied.Rows = slices.Clone(page.Rows)
	return &copied, nil
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
	copied := *page
	copied.Rows = slices.Clone(page.Rows)
	return &copied, nil
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
		return nil, errFakePeopleContentMessageNotFound
	}
	copied := *detail
	copied.Attachments = slices.Clone(detail.Attachments)
	return &copied, nil
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
	assert := assert.New(t)
	contact := contentTestContact()
	backend := &fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}
	model := peopleContentModel(backend, &contact)
	model.peopleState.meetingsErr = errors.New("old meetings failure")

	model.peopleState.tab = peopleTabFiles
	model.peopleState.filesErr = errors.New("active files failure")
	view := stripANSI(model.renderPeopleView())
	assert.Contains(view, "active files failure")
	assert.NotContains(view, "old meetings failure")

	model.peopleState.filesErr = nil
	model.peopleState.filesLoading = true
	view = stripANSI(model.renderPeopleView())
	assert.Contains(view, "Loading received files")
	assert.NotContains(view, "old meetings failure")

	model.peopleState.filesLoading = false
	model.peopleState.filesErr = errors.New("retry active files")
	model, cmd := sendKey(t, model, key('r'))
	retriedFiles := runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd)
	assert.Equal(contact.ID, retriedFiles.participantID)

	model.peopleState.tab = peopleTabActivity
	model.peopleState.activityErr = errors.New("active activity failure")
	view = stripANSI(model.renderPeopleView())
	assert.Contains(view, "active activity failure")
	assert.NotContains(view, "old meetings failure")

	model.peopleState.activityErr = nil
	model.peopleState.activityLoading = true
	view = stripANSI(model.renderPeopleView())
	assert.Contains(view, "Loading contact activity")
	assert.NotContains(view, "old meetings failure")
}

func TestPeopleContentDetailErrorsRetryTheSelectedItem(t *testing.T) {
	contact := contentTestContact()

	t.Run("meeting", func(t *testing.T) {
		assert := assert.New(t)
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
		assert.Contains(stripANSI(model.renderPeopleView()), "meeting detail failed")
		assert.Contains(stripANSI(model.renderPeopleView()), "r retry")

		model, cmd = sendKey(t, model, key('r'))
		retried := runPeopleCommandMessage[peopleMeetingLoadedMsg](t, cmd)
		model = sendMsg(t, model, retried)
		assert.Equal(peopleLevelMeetingDetail, model.peopleState.level)
		assert.Equal(messageID, model.peopleState.selectedContentMessage)
		assert.Contains(stripANSI(model.renderPeopleView()), "Recovered transcript")
		assert.Equal([]int64{messageID, messageID}, backend.detailRequests)
	})

	t.Run("activity", func(t *testing.T) {
		assert := assert.New(t)
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
		assert.Contains(stripANSI(model.renderPeopleView()), "activity detail failed")
		assert.Contains(stripANSI(model.renderPeopleView()), "r retry")

		model, cmd = sendKey(t, model, key('r'))
		retried := runPeopleCommandMessage[peopleActivityMessageLoadedMsg](t, cmd)
		model = sendMsg(t, model, retried)
		assert.Equal(peopleLevelActivityMessage, model.peopleState.level)
		assert.Equal(messageID, model.peopleState.selectedContentMessage)
		assert.Contains(stripANSI(model.renderPeopleView()), "Recovered message")
		assert.Equal([]int64{messageID, messageID}, backend.detailRequests)
	})

	t.Run("file", func(t *testing.T) {
		assert := assert.New(t)
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
		assert.Contains(stripANSI(model.renderPeopleView()), "file detail failed")
		assert.Contains(stripANSI(model.renderPeopleView()), "r retry")

		model, cmd = sendKey(t, model, key('r'))
		retried := runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, cmd)
		updatedModel, exportCmd := model.Update(retried)
		model = asModel(t, updatedModel)
		require.NotNil(t, exportCmd)
		assert.Equal(fileID, model.peopleState.selectedContentFile)
		assert.Equal(messageID, model.peopleState.selectedContentMessage)
		assert.Equal([]int64{messageID, messageID}, backend.detailRequests)
	})
}

func TestPeopleFileFailureDoesNotFollowCursorToAnotherFile(t *testing.T) {
	assert := assert.New(t)
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
	assert.Contains(stripANSI(model.renderPeopleView()), "file A failed")
	assert.Contains(stripANSI(model.renderPeopleView()), "r retry")

	model, _ = sendKey(t, model, keyDown())
	assert.Equal(1, model.peopleState.cursor)
	require.NoError(t, model.peopleState.filesErr)
	assert.False(model.peopleState.fileOpenFailed)
	assert.Zero(model.peopleState.selectedContentFile)
	assert.Zero(model.peopleState.selectedContentMessage)
	view := stripANSI(model.renderPeopleView())
	assert.NotContains(view, "file A failed")
	assert.NotContains(view, "r retry")

	model, retry := sendKey(t, model, key('r'))
	assert.Nil(retry)
	assert.Equal([]int64{messageA}, backend.detailRequests)

	_, cmd = sendKey(t, model, keyEnter())
	opened := runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, cmd)
	assert.Equal(fileB, opened.fileID)
	assert.Equal(messageB, opened.messageID)
}

func TestPeopleMeetingsUseExactParticipantAndExistingTranscriptReader(t *testing.T) {
	assert := assert.New(t)
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
	assert.Equal(peoplebrowser.ContactPageRequest{
		ParticipantID: contact.ID,
		Limit:         peoplePageSize,
	}, backend.meetingRequests[0])
	view := model.renderPeopleView()
	assert.Contains(view, "Newest sync")
	assert.Contains(view, "Older sync")
	assert.NotContains(view, "899")
	assert.Less(stringsIndex(view, "Newest sync"), stringsIndex(view, "Older sync"))

	model, cmd = sendKey(t, model, keyEnter())
	detailLoaded := runPeopleCommandMessage[peopleMeetingLoadedMsg](t, cmd)
	model = sendMsg(t, model, detailLoaded)

	assert.Equal([]int64{802}, backend.detailRequests)
	assert.Equal(peopleLevelMeetingDetail, model.peopleState.level)
	assert.Contains(stripANSI(model.renderPeopleView()), "needle in transcript")

	model, _ = sendKey(t, model, key('/'))
	assert.True(model.meetingState.detailSearchActive)
	model.meetingState.detailSearchInput.SetValue("needle")
	model, _ = sendKey(t, model, keyEnter())
	assert.Equal("needle", model.meetingState.detailSearchQuery)
	assert.NotEmpty(model.meetingState.detailSearchMatches)
}

func TestPeopleFilesPageDeduplicatesAndUsesAttachmentExportPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const contentHash = "abc123def456abc123def456abc123def456abc123def456abc123def456abc1"
	newest := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	contact := contentTestContact()
	backend := &fakePeopleContentBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		filePages: map[int64][]*peoplebrowser.FilePage{
			contact.ID: {
				{
					Rows: []query.FileRow{
						{ID: 80, MessageID: 880, OccurredAt: newest, Filename: "notes.pdf", MimeType: "application/pdf", Size: 2048, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp},
						{ID: 81, MessageID: 881, OccurredAt: newest.Add(-time.Hour), Filename: "photo.png", MimeType: "image/png", Size: 512, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp},
					},
					TotalCount: 3, NextCursor: "files-next", CacheRevision: "cache-8",
				},
				{
					Rows: []query.FileRow{
						{ID: 81, MessageID: 881, OccurredAt: newest.Add(-time.Hour), Filename: "photo.png"},
						{ID: 82, MessageID: 882, OccurredAt: newest.Add(-2 * time.Hour), Filename: "archive.zip", MimeType: "application/zip", Size: 4096, SourceType: emailMessageType, SourceIdentifier: "account@example.test"},
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
	outputDir := t.TempDir()
	model.actions = NewActionControllerWithOptions(model.engine, ActionControllerOptions{
		DataDir:             t.TempDir(),
		AttachmentOutputDir: outputDir,
		AttachmentReader: mapAttachmentReader{data: map[string][]byte{
			contentHash: []byte("attachment bytes"),
		}},
	})

	updated, cmd := model.activatePeopleTab(peopleTabFiles)
	model = asModel(t, updated)
	first := runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd)
	model = sendMsg(t, model, first)

	require.Len(backend.fileRequests, 1)
	assert.Equal(peoplebrowser.ContactPageRequest{
		ParticipantID: contact.ID,
		Limit:         peoplePageSize,
	}, backend.fileRequests[0])
	view := model.renderPeopleView()
	assert.Contains(view, "notes.pdf")
	assert.Contains(view, "application/pdf")
	assert.Contains(view, "2.0 KB")
	assert.Contains(view, "Beeper/WhatsApp")
	assert.NotContains(view, "outbound.txt")

	model, cmd = sendKey(t, model, keyDown())
	second := runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd)
	model = sendMsg(t, model, second)

	require.Len(backend.fileRequests, 2)
	assert.Equal("files-next", backend.fileRequests[1].Cursor)
	require.Len(model.peopleState.files, 3)
	assert.Equal([]int64{80, 81, 82}, []int64{
		model.peopleState.files[0].ID,
		model.peopleState.files[1].ID,
		model.peopleState.files[2].ID,
	})

	model, _ = sendKey(t, model, keyHome())
	model, cmd = sendKey(t, model, keyEnter())
	detailLoaded := runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, cmd)
	updatedModel, exportCmd := model.Update(detailLoaded)
	model = asModel(t, updatedModel)
	require.NotNil(exportCmd)

	exportMessage, ok := exportCmd().(peopleFileExportedMsg)
	require.True(ok)
	require.NoError(exportMessage.result.Err)
	model = sendMsg(t, model, exportMessage)
	assert.Equal(modalExportResult, model.modal)
	assert.False(model.loading)
	zipped, err := zip.OpenReader(filepath.Join(outputDir, "Shared notes_880.zip"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(zipped.Close()) })
	require.Len(zipped.File, 1)
	assert.Equal("notes.pdf", zipped.File[0].Name)
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
	assert := assert.New(t)
	require := require.New(t)
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
	require.NotNil(exportCmd)
	assert.True(model.peopleState.fileOpening)

	model, promoteCmd := sendKey(t, model, key('p'))
	require.NotNil(promoteCmd)
	assert.False(model.peopleState.fileOpening)
	assert.False(model.peopleState.fileOpenFailed)
	assert.Zero(model.peopleState.selectedContentFile)
	assert.Zero(model.peopleState.selectedContentMessage)
	assert.True(model.peopleState.promoting)
	require.Len(model.peopleState.files, 1)

	outputDir := t.TempDir()
	t.Chdir(outputDir)
	staleExport, ok := exportCmd().(peopleFileExportedMsg)
	require.True(ok)
	model = sendMsg(t, model, staleExport)
	assert.Equal(modalNone, model.modal)
	assert.False(model.peopleState.fileOpening)

	promoted := runPeopleCommandMessage[peoplePromotedMsg](t, promoteCmd)
	updatedModel, attributesCmd := model.Update(promoted)
	model = asModel(t, updatedModel)
	require.NotNil(attributesCmd)
	require.NotNil(model.peopleState.contact.Profile)
	assert.Equal(int64(51), model.peopleState.contact.Profile.ID)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleAttributesLoadedMsg](t, attributesCmd))

	assert.Equal(peopleTabFiles, model.peopleState.tab)
	assert.False(model.loading)
	assert.False(model.peopleState.fileOpening)
	require.Len(model.peopleState.files, 1)
	assert.Contains(stripANSI(model.renderPeopleView()), "promotion.txt")
	assert.NotContains(stripANSI(model.renderPeopleView()), "Loading received files")
	assert.Equal([]int64{contact.ID}, backend.promoteRequests)
	assert.Equal([]int64{51}, backend.attributeRequests)

	_, reopen := sendKey(t, model, keyEnter())
	reopened := runPeopleCommandMessage[peopleFileMessageLoadedMsg](t, reopen)
	assert.Equal(messageID, reopened.messageID)
	assert.Equal([]int64{messageID, messageID}, backend.detailRequests)
}

func TestPeopleMeetingDetailPreservesStandaloneMeetingReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.True(handled)
	model = entered
	require.Equal(modePeople, model.mode)

	updated, cmd := model.activatePeopleTab(peopleTabMeetings)
	model = asModel(t, updated)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, cmd))
	model, cmd = sendKey(t, model, keyEnter())
	model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingLoadedMsg](t, cmd))
	require.NotNil(model.meetingState.detail)
	assert.Equal(contactID, model.meetingState.detail.ID)
	model, _ = sendKey(t, model, key('/'))
	model.meetingState.detailSearchInput.SetValue("contact needle")
	model, _ = sendKey(t, model, keyEnter())
	assert.Equal("contact needle", model.meetingState.detailSearchQuery)

	left, _, handled := model.handleGlobalKeys(key('m'))
	require.True(handled)
	model = left
	require.Equal(modeEmail, model.mode)
	model.mode = modeTexts
	returned, _, handled := model.handleGlobalKeys(key('m'))
	require.True(handled)
	model = returned
	require.Equal(modeMeetings, model.mode)

	assert.Equal(meetingLevelDetail, model.meetingState.level)
	assert.Same(standaloneDetail, model.meetingState.detail)
	assert.Equal(3, model.meetingState.detailScroll)
	assert.Equal("standalone needle", model.meetingState.detailSearchQuery)
	assert.Equal("standalone needle", model.meetingState.detailSearchInput.Value())
	assert.Equal([]int{4}, model.meetingState.detailSearchMatches)
	assert.Contains(stripANSI(model.renderMeetingView()), "Standalone transcript")

	reentered, _, handled := model.handleGlobalKeys(key('m'))
	require.True(handled)
	model = reentered
	require.Equal(modePeople, model.mode)
	require.NotNil(model.meetingState.detail)
	assert.Equal(contactID, model.meetingState.detail.ID)
	model, _ = sendKey(t, model, keyEsc())
	assert.Equal(peopleLevelContact, model.peopleState.level)
	assert.Nil(model.meetingState.detail)

	left, _, handled = model.handleGlobalKeys(key('m'))
	require.True(handled)
	model = left
	model.mode = modeTexts
	returned, _, handled = model.handleGlobalKeys(key('m'))
	require.True(handled)
	model = returned
	assert.Same(standaloneDetail, model.meetingState.detail)
	assert.Equal("standalone needle", model.meetingState.detailSearchQuery)
}

func TestPeopleContentTabsAdvertiseImplementedNavigation(t *testing.T) {
	assert := assert.New(t)
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
		case peopleTabOverview, peopleTabAttributes, peopleTabInboxes, peopleTabCount:
		}

		wide := stripANSI(model.peopleFooterView())
		assert.Contains(wide, helpLabelVertical)
		assert.Contains(wide, "PgUp/PgDn")
		assert.Contains(wide, helpLabelEnter)
		assert.Contains(wide, "r retry")
		assert.Contains(wide, "Tab")

		model.width = 44
		narrow := stripANSI(model.peopleFooterView())
		assert.Contains(narrow, helpLabelVertical)
		assert.Contains(narrow, "Pg")
		assert.Contains(narrow, helpLabelEnter)
		assert.Contains(narrow, "r")
		assert.Contains(narrow, "Tab")
	}
}

func TestPeopleActivityGroupsLocalDaysPagesAndOpensExistingMessageReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
							SourceType: "gmail", MessageType: emailMessageType, Title: "Re: Plans",
							MatchedRecipientIdentities: []string{"contact@example.test"},
						},
						{
							Key: "message:902", Kind: query.EntryConversation, AnchorMessageID: &chatMessageID,
							OccurredAt: time.Date(2026, 8, 20, 23, 30, 0, 0, time.UTC),
							SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp,
							MessageType: sourceTypeWhatsApp, Title: "Morning",
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
						{Key: "email:900", Kind: query.EntryEmail, OccurredAt: time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC), SourceType: "gmail", MessageType: emailMessageType, Title: "Earlier note"},
					},
					TotalCount: 4, CacheRevision: "cache-8",
				},
			},
		},
		details: map[int64]*query.MessageDetail{
			chatMessageID: {
				ID: chatMessageID, Subject: "Morning", SentAt: time.Date(2026, 8, 20, 23, 30, 0, 0, time.UTC),
				MessageType: sourceTypeWhatsApp, BodyText: "Existing activity detail body",
			},
		},
	}
	model := peopleContentModel(backend, &contact)
	model.peopleState.location = local

	updated, cmd := model.activatePeopleTab(peopleTabActivity)
	model = asModel(t, updated)
	first := runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd)
	model = sendMsg(t, model, first)

	require.Len(backend.activityRequests, 1)
	assert.Equal(peoplebrowser.ActivityPageRequest{
		ParticipantID: contact.ID,
		Limit:         peoplePageSize,
	}, backend.activityRequests[0])
	view := model.renderPeopleView()
	assert.Contains(view, "2026-08-21")
	assert.Contains(view, "2026-08-20")
	assert.Contains(view, "● 01:30")
	assert.Contains(view, "▶ ● 01:30")
	assert.Contains(view, "│")
	assert.Contains(view, "WhatsApp")
	assert.Contains(view, "received")
	assert.Contains(view, "sent")
	assert.Contains(view, "Meeting")
	assert.Less(stringsIndex(view, "Morning"), stringsIndex(view, "Re: Plans"))

	model.width = 44
	narrow := model.renderPeopleView()
	assert.Contains(narrow, "2026-08-21")
	assert.Contains(narrow, "● 01:30")
	assert.Contains(narrow, "Morning")
	assert.NotContains(narrow, "WhatsApp")
	assert.NotContains(narrow, "received")

	model.width = 100
	model, cmd = sendKey(t, model, key('G'))
	second := runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd)
	model = sendMsg(t, model, second)
	require.Len(model.peopleState.activity, 4)
	assert.Equal([]string{"message:902", "meeting:903", "email:901", "email:900"}, []string{
		model.peopleState.activity[0].Key,
		model.peopleState.activity[1].Key,
		model.peopleState.activity[2].Key,
		model.peopleState.activity[3].Key,
	})

	model, _ = sendKey(t, model, keyHome())
	model, cmd = sendKey(t, model, keyEnter())
	detailLoaded := runPeopleCommandMessage[peopleActivityMessageLoadedMsg](t, cmd)
	model = sendMsg(t, model, detailLoaded)
	assert.Equal([]int64{chatMessageID}, backend.detailRequests)
	assert.Equal(peopleLevelActivityMessage, model.peopleState.level)
	assert.Contains(model.renderPeopleView(), "Existing activity detail body")
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
		assert := assert.New(t)
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
		assert.True(model.peopleState.meetingsRestarted)
		assert.Empty(model.peopleState.meetings)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, restartCmd))

		model, cmd = sendKey(t, model, keyDown())
		secondMismatch := runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, cmd)
		updatedModel, retryCmd := model.Update(secondMismatch)
		model = asModel(t, updatedModel)
		assert.Nil(retryCmd)
		require.ErrorIs(t, model.peopleState.meetingsErr, errPeopleContentChanged)
		assert.Contains(model.renderPeopleView(), "r retry")
		assert.Nil(model.maybeLoadMorePeopleMeetings())
		assert.Equal([]string{"", "meeting-a", "", "meeting-b"}, contentRequestCursors(backend.meetingRequests))
	})

	t.Run("files", func(t *testing.T) {
		assert := assert.New(t)
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
		assert.True(model.peopleState.filesRestarted)
		assert.Empty(model.peopleState.files)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleFilesLoadedMsg](t, restartCmd))

		model, cmd = sendKey(t, model, keyDown())
		secondMismatch := runPeopleCommandMessage[peopleFilesLoadedMsg](t, cmd)
		updatedModel, retryCmd := model.Update(secondMismatch)
		model = asModel(t, updatedModel)
		assert.Nil(retryCmd)
		require.ErrorIs(t, model.peopleState.filesErr, errPeopleContentChanged)
		assert.Contains(model.renderPeopleView(), "r retry")
		assert.Nil(model.maybeLoadMorePeopleFiles())
		assert.Equal([]string{"", "file-a", "", "file-b"}, contentRequestCursors(backend.fileRequests))
	})

	t.Run("activity", func(t *testing.T) {
		assert := assert.New(t)
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
		assert.True(model.peopleState.activityRestarted)
		assert.Empty(model.peopleState.activity)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleActivityLoadedMsg](t, restartCmd))

		model, cmd = sendKey(t, model, keyDown())
		secondMismatch := runPeopleCommandMessage[peopleActivityLoadedMsg](t, cmd)
		updatedModel, retryCmd := model.Update(secondMismatch)
		model = asModel(t, updatedModel)
		assert.Nil(retryCmd)
		require.ErrorIs(t, model.peopleState.activityErr, errPeopleContentChanged)
		assert.Contains(model.renderPeopleView(), "r retry")
		assert.Nil(model.maybeLoadMorePeopleActivity())
		assert.Equal([]string{"", "activity-a", "", "activity-b"}, contentRequestCursors(backend.activityRequests))
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
	assert := assert.New(t)
	require := require.New(t)
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
	require.NotNil(pendingFileCmd)
	assert.True(model.peopleState.filesLoadingMore)

	updated, pendingActivityCmd := model.activatePeopleTab(peopleTabActivity)
	model = asModel(t, updated)
	require.NotNil(pendingActivityCmd)
	assert.False(model.peopleState.filesLoading)
	assert.False(model.peopleState.filesLoadingMore)
	assert.False(model.peopleState.filesLoaded)

	staleFile := runPeopleCommandMessage[peopleFilesLoadedMsg](t, pendingFileCmd)
	model = sendMsg(t, model, staleFile)
	assert.Equal(peopleTabActivity, model.peopleState.tab)
	assert.Empty(model.peopleState.files)

	updated, reloadCmd := model.activatePeopleTab(peopleTabFiles)
	model = asModel(t, updated)
	require.NotNil(reloadCmd)
	reloaded := runPeopleCommandMessage[peopleFilesLoadedMsg](t, reloadCmd)
	model = sendMsg(t, model, reloaded)
	require.Len(model.peopleState.files, 1)
	assert.Equal(int64(3), model.peopleState.files[0].ID)
	assert.Equal([]string{"old-next", ""}, contentRequestCursors(backend.fileRequests))

	staleActivity := runPeopleCommandMessage[peopleActivityLoadedMsg](t, pendingActivityCmd)
	model = sendMsg(t, model, staleActivity)
	assert.Equal(peopleTabFiles, model.peopleState.tab)
	assert.Empty(model.peopleState.activity)
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
	assert := assert.New(t)
	require := require.New(t)
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
	require.NotNil(pending)
	model, fresh := leaveAndReenterPeople(t, model)

	require.NotNil(fresh)
	assert.True(model.peopleState.meetingsLoading)
	stale := runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, pending)
	model = sendMsg(t, model, stale)
	assert.Empty(model.peopleState.meetings)
	loaded := runPeopleCommandMessage[peopleMeetingsLoadedMsg](t, fresh)
	model = sendMsg(t, model, loaded)
	require.Len(model.peopleState.meetings, 1)
	assert.Equal("Fresh meeting", model.peopleState.meetings[0].Subject)
	assert.Equal([]string{"", ""}, contentRequestCursors(backend.meetingRequests))
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
		assert := assert.New(t)
		require := require.New(t)
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
		require.NotNil(fresh)
		assert.True(model.meetingState.detailLoading)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingLoadedMsg](t, pending))
		assert.Nil(model.meetingState.detail)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleMeetingLoadedMsg](t, fresh))
		require.NotNil(model.meetingState.detail)
		assert.Equal("Fresh transcript", model.meetingState.detail.BodyText)
		assert.Equal([]int64{messageID, messageID}, backend.detailRequests)
	})

	t.Run("activity message", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
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
		require.NotNil(fresh)
		assert.True(model.peopleState.messageLoading)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleActivityMessageLoadedMsg](t, pending))
		assert.Nil(model.messageDetail)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleActivityMessageLoadedMsg](t, fresh))
		require.NotNil(model.messageDetail)
		assert.Equal("Fresh activity body", model.messageDetail.BodyText)
		assert.Equal([]int64{messageID, messageID}, backend.detailRequests)
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
	assert.Regexp(t, `(?m)^.*▶ ● .*Activity 7`, view)
}

func stringsIndex(haystack, needle string) int {
	return strings.Index(haystack, needle)
}
