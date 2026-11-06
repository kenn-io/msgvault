package muesli

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/store"
	"hegel.dev/go/hegel"
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

func TestRemoteMeetingPlatformPreservesLocalSnapshot(t *testing.T) {
	// This is the complete accepted remote domain, including future identifiers,
	// whitespace and line separators. Controls are covered in the reject direction.
	hegel.Test(t, func(ht *hegel.T) {
		platform := hegel.Draw(ht, hegel.Text().MaxSize(128).ExcludeCategories([]string{"Cc", "Cs"}))
		meeting := sampleMeeting()
		meeting.ContactsState = ContactsOff
		meeting.Participants = meeting.Participants[:2] // already-normalized remote identities
		meeting.MeetingPlatform = platform
		local, err := meeting.ArchiveSnapshot(7, "mac", "you@example.com")
		require.NoError(ht, err)
		request := RemoteRequest{Action: "upsert", Source: meetingimport.Source{Identifier: "mac", AccountEmail: "you@example.com"}, Meeting: NewRemoteMeeting(meeting)}
		assert.Equal(ht, platform, request.Meeting.Record.MeetingPlatform)
		encoded, err := json.Marshal(request)
		require.NoError(ht, err)
		decoded, err := DecodeRemoteRequest(bytes.NewReader(encoded), MaxRemoteRequestBytes)
		require.NoError(ht, err)
		assert.Equal(ht, platform, decoded.Meeting.toMeeting().MeetingPlatform)
		remote, err := decoded.Meeting.toMeeting().ArchiveSnapshot(7, "mac", "you@example.com")
		require.NoError(ht, err)
		assert.Equal(ht, "meeting:42:20260901T140003Z", remote.SourceMessageID)
		assert.Equal(ht, local.Raw, remote.Raw)
		assert.Equal(ht, local.Metadata, remote.Metadata)
		assert.Equal(ht, local.Body, remote.Body)
	})
}

func TestRemoteMeetingPlatformValidationDomain(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		platform := hegel.Draw(ht, hegel.Text().ExcludeCategories([]string{"Cs"}))
		r := remoteTestRequest()
		r.Meeting.Record.MeetingPlatform = platform
		normal, err := r.Normalize()
		if utf8.RuneCountInString(platform) > 128 || strings.IndexFunc(platform, unicode.IsControl) >= 0 {
			assert.ErrorIs(ht, err, ErrRemoteValidation)
		} else {
			require.NoError(ht, err)
			assert.Equal(ht, platform, normal.Meeting.Record.MeetingPlatform)
		}
	})
}

func TestRemoteMeetingPlatformBoundaries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, value := range []string{"", "unknown", " FutureCall.v2 ", "  ", "Future\u2028Call\u2029", "Future\u200dCall"} {
		r := remoteTestRequest()
		r.Meeting.Record.MeetingPlatform = value
		encoded, err := json.Marshal(r)
		require.NoError(err)
		decoded, err := DecodeRemoteRequest(bytes.NewReader(encoded), MaxRemoteRequestBytes)
		require.NoError(err)
		assert.Equal(value, decoded.Meeting.Record.MeetingPlatform)
		if value == "" {
			assert.NotContains(string(encoded), "meeting_platform", "old recorder payload remains valid")
		}
	}
	invalid := remoteTestRequest()
	invalid.Meeting.Record.MeetingPlatform = string([]byte{0xff})
	_, err := invalid.Normalize()
	require.ErrorIs(err, ErrRemoteValidation)
	assert.NotContains(err.Error(), invalid.Meeting.Record.MeetingPlatform)
	for _, value := range []string{`123`, `true`, `[]`, `{}`} {
		r := remoteTestRequest()
		data, err := json.Marshal(r)
		require.NoError(err)
		data = bytes.Replace(data, []byte(`"record":{`), []byte(`"record":{"meeting_platform":`+value+`,`), 1)
		_, err = DecodeRemoteRequest(bytes.NewReader(data), MaxRemoteRequestBytes)
		require.ErrorIs(err, ErrRemoteMalformed)
	}
}

func TestRemoteMeetingPlatformCountsCodePoints(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, count := range []int{127, 128, 129} {
		for _, value := range []string{
			strings.Repeat("会", count),
			strings.Repeat("😀", count),
			strings.Repeat("e\u0301", count/2) + strings.Repeat("e", count%2),
		} {
			r := remoteTestRequest()
			r.Meeting.Record.MeetingPlatform = value
			_, normalizeErr := r.Normalize()
			encoded, err := json.Marshal(r)
			require.NoError(err)
			decoded, decodeErr := DecodeRemoteRequest(bytes.NewReader(encoded), MaxRemoteRequestBytes)
			if count > 128 {
				require.ErrorIs(normalizeErr, ErrRemoteValidation)
				require.ErrorIs(decodeErr, ErrRemoteValidation)
			} else {
				require.NoError(normalizeErr)
				require.NoError(decodeErr)
				assert.Equal(value, decoded.Meeting.toMeeting().MeetingPlatform)
			}
		}
	}
}

func TestRemoteMeetingPlatformRejectsEveryC0AndC1Control(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// Exhaust the C0/C1 ranges at every position; a broad text generator alone
	// cannot guarantee hitting NUL, DEL, NEL or each end of both ranges.
	for control := rune(0); control <= '\u009f'; control++ {
		if control >= '\u0020' && control < '\u007f' {
			continue
		}
		for _, value := range []string{string(control) + "Future", "Fu" + string(control) + "ture", "Future" + string(control)} {
			r := remoteTestRequest()
			r.Meeting.Record.MeetingPlatform = value
			_, err := r.Normalize()
			require.ErrorIs(err, ErrRemoteValidation, "control %U", control)
			assert.NotContains(err.Error(), value)
			encoded, err := json.Marshal(r)
			require.NoError(err)
			_, err = DecodeRemoteRequest(bytes.NewReader(encoded), MaxRemoteRequestBytes)
			require.ErrorIs(err, ErrRemoteValidation, "escaped control %U", control)
			assert.NotContains(err.Error(), value)
		}
	}
}

func TestRemoteMeetingPlatformRejectedEditPreservesArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	r := remoteTestRequest()
	r.Meeting.Record.Source = "meeting"
	r.Meeting.Record.MeetingPlatform = "facetime"
	first, err := f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	runs := remotePushRuns(t, f)
	for _, value := range []string{"Future\x1fCall", "Future\u009fCall", strings.Repeat("会", 129)} {
		r.Meeting.Record.MeetingPlatform = value
		_, err := f.imp.ImportRemote(t.Context(), r)
		require.ErrorIs(err, ErrRemoteValidation)
		assertStoredMeetingPlatform(t, f, first.MessageID, "facetime")
		assert.Equal(1, f.messageCount(t))
		assert.Equal(runs, remotePushRuns(t, f), "rejection must not open a write run")
	}
}

func TestRemoteScanMeetingPlatformEditRefreshesArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	_, err := f.muesli.ExecContext(t.Context(), `ALTER TABLE meetings ADD COLUMN meeting_platform TEXT`)
	require.NoError(err)
	id := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir()}
	var results []RemoteResult
	var digests []string
	send := func(ctx context.Context, r RemoteRequest) (RemoteResult, error) {
		digest, err := meetingDigest(r.Meeting)
		require.NoError(err)
		digests = append(digests, digest)
		result, err := f.imp.ImportRemote(ctx, r)
		results = append(results, result)
		return result, err
	}
	_, err = ScanRemote(t.Context(), opts, send)
	require.NoError(err)
	require.Len(results, 1)
	messageID := results[0].MessageID
	for i, value := range []string{"unknown", " FutureCall.会議 ", ""} {
		_, err := f.muesli.ExecContext(t.Context(), `UPDATE meetings SET meeting_platform = ? WHERE id = ?`, value, id)
		require.NoError(err)
		_, err = ScanRemote(t.Context(), opts, send)
		require.NoError(err)
		require.Len(results, i+2)
		assert.Equal("updated", results[i+1].Status)
		assert.Equal(messageID, results[i+1].MessageID)
		assert.NotEqual(digests[i], digests[i+1])
		assertStoredMeetingPlatform(t, f, messageID, value)
		_, err = ScanRemote(t.Context(), opts, send)
		require.NoError(err)
		assert.Len(results, i+2, "acknowledged unchanged evidence is not resent")
	}
	assert.Equal(1, f.messageCount(t))
}

func TestRemoteScanSkipsInvalidPlatformAndSyncsMeetingChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	_, err := f.muesli.ExecContext(t.Context(), `ALTER TABLE meetings ADD COLUMN meeting_platform TEXT`)
	require.NoError(err)
	id := insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"meeting_platform": "facetime"}))
	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir()}
	var requests []RemoteRequest
	var results []RemoteResult
	send := func(ctx context.Context, request RemoteRequest) (RemoteResult, error) {
		requests = append(requests, request)
		result, err := f.imp.ImportRemote(ctx, request)
		if err == nil {
			results = append(results, result)
		}
		return result, err
	}
	_, err = ScanRemote(t.Context(), opts, send)
	require.NoError(err)
	require.Len(requests, 1)
	require.Len(results, 1)
	messageID := results[0].MessageID

	controlValue := "\x00FutureCall"
	_, err = f.muesli.ExecContext(t.Context(), `UPDATE meetings SET meeting_platform = ?, title = ? WHERE id = ?`, controlValue, "Updated synthetic meeting", id)
	require.NoError(err)
	_, err = ScanRemote(t.Context(), opts, send)
	require.NoError(err)
	require.Len(requests, 2)
	require.Len(results, 2)
	assert.Empty(requests[1].Meeting.Record.MeetingPlatform)
	assert.Equal("Updated synthetic meeting", requests[1].Meeting.Record.Title)
	assert.Equal("updated", results[1].Status)
	assert.Equal(messageID, results[1].MessageID)
	assertStoredMeetingPlatform(t, f, messageID, "")

	var stored string
	require.NoError(f.muesli.QueryRowContext(t.Context(), `SELECT meeting_platform FROM meetings WHERE id = ?`, id).Scan(&stored))
	assert.Equal(controlValue, stored)
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
		source, err := f.st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, identifier)
		require.NoError(err)
		return source
	}

	owner, err := f.st.GetOrCreateSource("mbox", "alias-owner@example.test")
	require.NoError(err)
	alias := "Support"
	_, err = f.st.UpdateSourceSettingsContext(t.Context(), owner.ID, store.SourceSettingsUpdate{Alias: &alias})
	require.NoError(err)
	for range 2 {
		conflict := register("support-recorder", "Support")
		assert.Empty(conflict.DisplayName.String)
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
	body, err := f.st.GetMessageBodyText(t.Context(), first.MessageID)
	require.NoError(err)
	assert.Contains(body, "Late synthetic notes")
	assert.Contains(body, "Edited synthetic transcript")
	var attendees int
	require.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), first.MessageID).Scan(&attendees))
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
	_, err = f.muesli.ExecContext(t.Context(), `UPDATE meeting_participants SET participant_identifier = 'contact:CARD-A'`)
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
	prior, err := f.imp.previousParticipants(t.Context(), f.source.ID, key)
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
	require.NoError(t, f.st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sync_runs WHERE sync_type = 'muesli_push'`).Scan(&runs))
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
	_, err := f.muesli.ExecContext(t.Context(), `UPDATE meetings SET raw_transcript = 'Edited synthetic transcript' WHERE id = ?`, id)
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

func TestLoadRemoteUploadsInvalidatesIDOnlyRecordingAcknowledgements(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "uploads.json")
	legacy := `{"version":1,"meetings":{"1":"meeting-digest:recording-hash"},"recordings":{"1":"recording-hash"}}`
	require.NoError(os.WriteFile(path, []byte(legacy), 0600))

	uploads, err := loadRemoteUploads(path)
	require.NoError(err)
	assert.True(uploads.dirty)
	assert.Equal("meeting-digest:recording-hash", uploads.Meetings[1], "meeting acknowledgements remain usable")
	assert.False(uploads.recordingAcknowledged("meeting:1:20260901T140003Z", "recording-hash"), "row-ID-only recording acknowledgements are ambiguous")
	require.NoError(uploads.save())

	data, err := os.ReadFile(path)
	require.NoError(err)
	var migrated remoteUploads
	require.NoError(json.Unmarshal(data, &migrated))
	assert.Equal(remoteUploadsVersion, migrated.Version)
	assert.Empty(migrated.Recordings)
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
	_, err := f.muesli.ExecContext(t.Context(), `UPDATE meetings SET manual_notes = 'Newest synthetic notes'`)
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
	_, err = f.imp.store.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "canceled-recorder")
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
