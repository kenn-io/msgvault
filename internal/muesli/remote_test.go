package muesli

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/store"
)

func remoteTestRequest() RemoteRequest {
	return RemoteRequest{
		Action: "upsert", Source: meetingimport.Source{Identifier: "mac", AccountEmail: "you@example.com"},
		Meeting: NewRemoteMeeting(Meeting{ID: 42, Title: "Planning", Status: "completed",
			CreatedAt: "2026-09-01 14:00:03", StartTime: "2026-09-01T14:00:00Z",
			RawTranscript: "Synthetic transcript", ContactsState: ContactsOff,
			Participants: []Participant{{Name: "Test Attendee", Email: "attendee@example.com", Identifier: "email:attendee@example.com"}},
		}),
	}
}

func registerRemoteFixture(t *testing.T, f *importerFixture) {
	t.Helper()
	r := remoteTestRequest()
	r.Action, r.Meeting = "register", nil
	result, err := f.imp.ImportRemote(t.Context(), r)
	require.NoError(t, err)
	assert.Equal(t, "registered", result.Status)
}

func TestRemoteRegistrationUsesOptionalDisplayName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	register := func(identifier, displayName string) *store.Source {
		r := RemoteRequest{
			Action: "register",
			Source: meetingimport.Source{
				Identifier: identifier, DisplayName: displayName, AccountEmail: "you@example.com",
			},
		}
		result, err := f.imp.ImportRemote(t.Context(), r)
		require.NoError(err)
		assert.Equal("registered", result.Status)
		source, err := f.st.GetSourceByTypeAndIdentifier(SourceType, identifier)
		require.NoError(err)
		return source
	}

	defaulted := register("default-recorder", "")
	require.True(defaulted.DisplayName.Valid)
	assert.Equal("default-recorder", defaulted.DisplayName.String)

	custom := register("custom-recorder", "  Synthetic Recorder  ")
	require.True(custom.DisplayName.Valid)
	assert.Equal("Synthetic Recorder", custom.DisplayName.String)
	retried := register("custom-recorder", "")
	require.True(retried.DisplayName.Valid)
	assert.Equal("Synthetic Recorder", retried.DisplayName.String, "an omitted retry must preserve the registered label")
}

func TestRemoteRoundTripAndEdits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	r := remoteTestRequest()
	encoded, err := json.Marshal(r)
	require.NoError(err)
	assert.NotContains(string(encoded), "email:attendee@example.com")
	decoded, err := DecodeRemoteRequest(bytes.NewReader(encoded), MaxRemoteRequestBytes)
	require.NoError(err)
	first, err := f.imp.ImportRemote(t.Context(), decoded)
	require.NoError(err)
	assert.Equal("created", first.Status)
	retry, err := f.imp.ImportRemote(t.Context(), decoded) // Also repairs a lost acknowledgement.
	require.NoError(err)
	assert.Equal(first.MessageID, retry.MessageID)
	assert.Equal("unchanged", retry.Status)
	assert.False(retry.Changed)
	r.Meeting.Record.ManualNotes = "Late synthetic notes"
	r.Meeting.Record.RawTranscript = "Edited synthetic transcript"
	r.Meeting.Participants = nil
	updated, err := f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	assert.Equal(first.MessageID, updated.MessageID)
	assert.Equal("updated", updated.Status)
	body, err := f.st.GetMessageBodyText(first.MessageID)
	require.NoError(err)
	assert.Contains(body, "Late synthetic notes")
	assert.Contains(body, "Edited synthetic transcript")
	var attendees int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), first.MessageID).Scan(&attendees))
	assert.Zero(attendees)
	r.Source.AccountEmail = "another@example.com"
	_, err = f.imp.ImportRemote(t.Context(), r)
	assert.ErrorIs(err, ErrRemoteValidation)
}

func TestRemoteValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*RemoteRequest)
	}{
		{"source", func(r *RemoteRequest) { r.Source.Identifier = strings.Repeat("x", 129) }},
		{"email", func(r *RemoteRequest) { r.Source.AccountEmail = "private-invalid-value" }},
		{"status", func(r *RemoteRequest) { r.Meeting.Record.Status = "recording" }},
		{"negative_id", func(r *RemoteRequest) { r.Meeting.Record.ID = -1 }},
		{"timestamp", func(r *RemoteRequest) { r.Meeting.Record.CreatedAt = "private-invalid-value" }},
		{"end_timestamp", func(r *RemoteRequest) { r.Meeting.Record.EndTime = "private-invalid-value" }},
		{"reversed_end", func(r *RemoteRequest) { r.Meeting.Record.EndTime = "2026-09-01T13:00:00Z" }},
		{"duration_overflow", func(r *RemoteRequest) { r.Meeting.Record.DurationSeconds = 1e30 }},
		{"attendees", func(r *RemoteRequest) { r.Meeting.Participants = make([]RemoteParticipant, 201) }},
		{"anchor_off", func(r *RemoteRequest) { r.Meeting.Participants[0].Anchor = "apple-contact:" + strings.Repeat("a", 64) }},
		{"raw_ref", func(r *RemoteRequest) { r.Meeting.Participants[0].Ref = "private-invalid-value" }},
		{"cache_flags", func(r *RemoteRequest) { r.BuildCache, r.NoBuildCache = true, true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := remoteTestRequest()
			tc.change(&r)
			_, err := r.Normalize()
			require.ErrorIs(t, err, ErrRemoteValidation)
			assert.NotContains(t, err.Error(), "private-invalid-value")
		})
	}
	for _, body := range []string{`{"unknown":true}`, `{"action":"register","action":"upsert"}`, `{} {}`, `{"source":null}`} {
		_, err := DecodeRemoteRequest(strings.NewReader(body), MaxRemoteRequestBytes)
		require.Error(t, err)
	}
	_, err := DecodeRemoteRequest(strings.NewReader(strings.Repeat(" ", 65)), 64)
	assert.ErrorIs(t, err, ErrRemoteTooLarge)
}

func TestRemoteContactsAmbiguityAndCarry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	root := t.TempDir()
	newAddressBookStore(t, filepath.Join(root, addressBookFile),
		fixtureCard{uniqueID: "CARD-A", emails: []string{"attendee@example.com"}, phones: []string{"+16045550100"}},
		fixtureCard{uniqueID: "CARD-B", emails: []string{"attendee@example.com"}, phones: []string{"+16045550101"}},
	)
	id := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meeting_participants", map[string]any{"meeting_id": id,
		"participant_identifier": "email:attendee@example.com", "display_name": "Test Attendee", "email_address": "attendee@example.com", "insertion_order": 0})
	var sent RemoteRequest
	send := func(ctx context.Context, r RemoteRequest) (RemoteResult, error) {
		sent = r
		return f.imp.ImportRemote(ctx, r)
	}
	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path,
		ContactsEnabled: true, ContactsPath: root, LockDir: t.TempDir()}
	_, err := ScanRemote(t.Context(), opts, send)
	require.NoError(err)
	require.Len(sent.Meeting.Participants, 1)
	assert.Empty(sent.Meeting.Participants[0].Anchor)
	assert.Empty(sent.Meeting.Participants[0].Phones)
	// An explicit card reference can resolve; the raw Contacts ID never transfers.
	_, err = f.muesli.Exec(`UPDATE meeting_participants SET participant_identifier = 'contact:CARD-A'`)
	require.NoError(err)
	_, err = ScanRemote(t.Context(), opts, send)
	require.NoError(err)
	assert.NotEmpty(sent.Meeting.Participants[0].Anchor)
	raw, err := json.Marshal(sent)
	require.NoError(err)
	assert.NotContains(string(raw), "CARD-A")
	opts.ContactsPath = filepath.Join(t.TempDir(), "missing")
	_, err = ScanRemote(t.Context(), opts, send)
	require.NoError(err)
	key, err := sent.Meeting.toMeeting().SourceMessageID()
	require.NoError(err)
	prior, err := f.imp.previousParticipants(f.source.ID, key)
	require.NoError(err)
	require.Len(prior, 1)
	for _, p := range prior {
		assert.Equal([]string{"+16045550100"}, p.Phones)
	}
}

func TestRemoteScanContinuesAfterInvalidMeeting(t *testing.T) {
	assert := assert.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	bad := insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"created_at": "bad"}))
	insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	sum, err := ScanRemote(t.Context(), ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir()}, f.imp.ImportRemote)
	require.Error(t, err)
	require.ErrorContains(t, err, fmt.Sprintf("muesli meeting %d:", bad), "the error names the meeting to inspect")
	assert.Equal(int64(1), sum.Errors)
	assert.Equal(int64(1), sum.MeetingsAdded)
	assert.Equal(1, f.messageCount(t))
}

func remotePushRuns(t *testing.T, f *importerFixture) int {
	t.Helper()
	var runs int
	require.NoError(t, f.st.DB().QueryRow(`SELECT count(*) FROM sync_runs WHERE sync_type = 'muesli_push'`).Scan(&runs))
	return runs
}

func TestRemoteUnchangedPushOpensNoSyncRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	r := remoteTestRequest()
	first, err := f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	require.Equal("created", first.Status)
	for range 3 {
		retry, err := f.imp.ImportRemote(t.Context(), r)
		require.NoError(err)
		assert.Equal("unchanged", retry.Status)
		assert.Equal(first.MessageID, retry.MessageID)
	}
	assert.Equal(1, remotePushRuns(t, f), "retransmissions must not grow sync history")
	r.Full = true
	forced, err := f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	assert.Equal("updated", forced.Status)
	assert.Equal(2, remotePushRuns(t, f), "a forced rewrite is recorded")
}

func TestRemoteScanUploadsOnlyNewOrEditedMeetings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	id := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir(), RemoteTarget: "https://archive.example.com"}
	sends := 0
	send := func(ctx context.Context, r RemoteRequest) (RemoteResult, error) {
		sends++
		return f.imp.ImportRemote(ctx, r)
	}
	scan := func(opts ImportOptions) int {
		t.Helper()
		before := sends
		_, err := ScanRemote(t.Context(), opts, send)
		require.NoError(err)
		return sends - before
	}
	assert.Equal(1, scan(opts), "first scan uploads")
	assert.Equal(0, scan(opts), "acknowledged meeting is not uploaded again")
	_, err := f.muesli.Exec(`UPDATE meetings SET raw_transcript = 'Edited synthetic transcript' WHERE id = ?`, id)
	require.NoError(err)
	assert.Equal(1, scan(opts), "an edit is uploaded")
	assert.Equal(0, scan(opts))
	full := opts
	full.Full = true
	assert.Equal(1, scan(full), "--full uploads acknowledged meetings")
	other := opts
	other.RemoteTarget = "https://other-archive.example.com"
	assert.Equal(1, scan(other), "another daemon has its own record")
	require.NoError(ForgetRemoteUploads(t.Context(), opts))
	assert.Equal(1, scan(opts), "registration clears the record")
	assert.Equal(0, scan(opts))
}

func TestRemoteScanRetriesUnacknowledgedUpload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir()}
	unreachable := errors.New("synthetic network failure")
	_, err := ScanRemote(t.Context(), opts, func(context.Context, RemoteRequest) (RemoteResult, error) {
		return RemoteResult{}, unreachable
	})
	require.ErrorIs(err, unreachable)
	sends := 0
	_, err = ScanRemote(t.Context(), opts, func(ctx context.Context, r RemoteRequest) (RemoteResult, error) {
		sends++
		return f.imp.ImportRemote(ctx, r)
	})
	require.NoError(err)
	assert.Equal(1, sends, "a failed upload is offered again")
}

func TestRemoteScanReoffersMeetingsWithContactReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newResolveFixture(t,
		fixtureCard{uniqueID: "CARD-A:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+16045550100"}},
		fixtureCard{uniqueID: "CARD-B:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+16045550101"}},
	)
	id := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meeting_participants", map[string]any{"meeting_id": id, "participant_identifier": "email:attendee@example.com", "email_address": "attendee@example.com", "display_name": "Meeting Attendee", "source": "calendar", "insertion_order": 0})
	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, ContactsEnabled: true, ContactsPath: f.contacts, LockDir: t.TempDir()}
	sends := 0
	for range 2 {
		_, err := ScanRemote(t.Context(), opts, func(context.Context, RemoteRequest) (RemoteResult, error) {
			sends++
			return RemoteResult{Status: "unchanged"}, nil
		})
		require.NoError(err)
	}
	assert.Equal(2, sends, "a suggested phone may reach the archive after the last upload")
}

func TestRemoteScanSerializesBeforeRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir()}
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	firstDone := make(chan error, 1)
	go func() {
		_, err := ScanRemote(t.Context(), opts, func(ctx context.Context, r RemoteRequest) (RemoteResult, error) {
			close(entered)
			<-release
			return f.imp.ImportRemote(ctx, r)
		})
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow("first scan did not reach send")
	}
	_, err := f.muesli.Exec(`UPDATE meetings SET manual_notes = 'Newest synthetic notes'`)
	require.NoError(err)
	waitCtx, stopWaiting := context.WithTimeout(t.Context(), time.Second)
	defer stopWaiting()
	_, err = ScanRemote(waitCtx, opts, f.imp.ImportRemote)
	require.ErrorIs(err, context.DeadlineExceeded, "another process must wait before capturing its snapshot")
	secondDone := make(chan error, 1)
	go func() { _, err := ScanRemote(t.Context(), opts, f.imp.ImportRemote); secondDone <- err }()
	close(release)
	require.NoError(<-firstDone)
	require.NoError(<-secondDone)
	assert.Contains(f.body(t, "meeting:1:20260901T140003Z"), "Newest synthetic notes")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = ScanRemote(ctx, opts, f.imp.ImportRemote)
	assert.ErrorIs(err, context.Canceled)
}

func FuzzDecodeRemoteRequest(f *testing.F) {
	f.Add([]byte(`{"action":"register","source":{"identifier":"mac","account_email":"you@example.com"}}`))
	f.Add([]byte(`{"meeting":{"record":{"id":-1}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		require := require.New(t)
		r, err := DecodeRemoteRequest(bytes.NewReader(data), MaxRemoteRequestBytes)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(r)
		require.NoError(err)
		roundTrip, err := DecodeRemoteRequest(bytes.NewReader(encoded), MaxRemoteRequestBytes)
		require.NoError(err)
		reencoded, err := json.Marshal(roundTrip)
		require.NoError(err)
		assert.Equal(t, encoded, reencoded, "accepted DTOs preserve their normalized wire representation")
	})
}

func FuzzRemoteRequestSize(f *testing.F) {
	f.Add([]byte(`{}`), uint16(1))
	f.Add([]byte(`{}`), uint16(0))
	f.Fuzz(func(t *testing.T, data []byte, limit uint16) {
		if limit != 0 && len(data) <= int(limit) {
			return
		}
		_, err := DecodeRemoteRequest(bytes.NewReader(data), int64(limit))
		assert.ErrorIs(t, err, ErrRemoteTooLarge)
	})
}

func FuzzRemoteRejectsUnknownMembers(f *testing.F) {
	f.Add("synthetic value")
	f.Add("\xff")
	f.Fuzz(func(t *testing.T, value string) {
		data, err := json.Marshal(map[string]any{"action": "register", "source": map[string]string{"identifier": "mac", "account_email": "you@example.com"}, "unrecognized": value}, jsontext.AllowInvalidUTF8(true))
		require.NoError(t, err)
		_, err = DecodeRemoteRequest(bytes.NewReader(data), MaxRemoteRequestBytes)
		assert.Error(t, err, "unknown members must be rejected, whatever their value")
	})
}

func TestRemoteIdentityLimitIncludesPrimary(t *testing.T) {
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	p := &r.Meeting.Participants[0]
	p.Resolution = resolutionResolved
	for i := range 49 {
		p.Emails = append(p.Emails, fmt.Sprintf("identity%d@example.com", i))
	}
	_, err := r.Normalize()
	require.NoError(t, err)
	p.Emails = append(p.Emails, "last@example.com")
	_, err = r.Normalize()
	require.ErrorIs(t, err, ErrRemoteValidation)
	p.Emails[49] = p.Email
	_, err = r.Normalize()
	require.NoError(t, err, "duplicate primary does not add an identity")
}

func TestRemoteCanceledRegistrationDoesNotCreateSource(t *testing.T) {
	f := newImporterFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.imp.ImportRemote(ctx, RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: "canceled-recorder", AccountEmail: "user@example.com"}})
	require.ErrorIs(t, err, context.Canceled)
	_, err = f.imp.store.GetSourceByTypeAndIdentifier(SourceType, "canceled-recorder")
	require.ErrorIs(t, err, store.ErrSourceNotFound)
}

func TestRemoteBuildCacheRequiresRefreshAction(t *testing.T) {
	request := RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}, BuildCache: true}
	_, err := request.Normalize()
	require.ErrorIs(t, err, ErrRemoteValidation)
	request.Action = "refresh"
	_, err = request.Normalize()
	require.NoError(t, err)
}

func TestRemoteScanContinuesAfterInvalidUTF8Meeting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"raw_transcript": "Synthetic invalid UTF-8: \xff"}))
	insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	sum, err := ScanRemote(t.Context(), ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir()}, f.imp.ImportRemote)
	require.ErrorIs(err, ErrRemoteValidation)
	assert.Equal(int64(1), sum.Errors)
	assert.Equal(int64(1), sum.MeetingsAdded)
	assert.Equal(1, f.messageCount(t))
}

func FuzzRemoteNormalizedTextIsEncodable(f *testing.F) {
	for field := range uint8(9) {
		f.Add(field, "Synthetic text")
		f.Add(field, "\xff")
	}
	f.Fuzz(func(t *testing.T, field uint8, value string) {
		require := require.New(t)
		r := remoteTestRequest()
		switch field % 9 {
		case 0:
			r.Meeting.Record.RawTranscript = value
		case 1:
			r.Meeting.Record.FormattedNotes = value
		case 2:
			r.Meeting.Record.ManualNotes = value
		case 3:
			r.Meeting.Participants[0].Name = value
		case 4:
			r.Meeting.Participants[0].Source = value
		case 5:
			r.Source.DisplayName = value
		case 6:
			r.Meeting.Participants = append(r.Meeting.Participants, RemoteParticipant{Email: r.Meeting.Participants[0].Email, Name: value})
		case 7:
			r.Meeting.Participants = append(r.Meeting.Participants, RemoteParticipant{Email: r.Meeting.Participants[0].Email, Source: value})
		case 8:
			r.Meeting.Participants = append(r.Meeting.Participants, RemoteParticipant{Source: value})
		}
		normalized, err := r.Normalize()
		if err != nil {
			require.ErrorIs(err, ErrRemoteValidation)
			return
		}
		_, err = json.Marshal(normalized)
		require.NoError(err, "accepted normalized text must be encodable")
	})
}

func TestRemoteScanContinuesAfterInvalidDuplicateParticipant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	id := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	for order, name := range []string{"Synthetic Attendee", "Synthetic invalid UTF-8: \xff"} {
		insertRow(t, f.muesli, "meeting_participants", map[string]any{"meeting_id": id,
			"participant_identifier": fmt.Sprintf("synthetic:%d", order), "display_name": name,
			"email_address": "attendee@example.com", "insertion_order": order})
	}
	insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	sum, err := ScanRemote(t.Context(), ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir()}, f.imp.ImportRemote)
	require.ErrorIs(err, ErrRemoteValidation)
	assert.Equal(int64(1), sum.Errors)
	assert.Equal(int64(1), sum.MeetingsAdded)
	assert.Equal(1, f.messageCount(t))
}
