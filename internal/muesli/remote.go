package muesli

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/store"
)

const MaxRemoteRequestBytes = meetingimport.MaxRequestBytes

var (
	ErrRemoteValidation = errors.New("muesli transfer failed validation")
	ErrRemoteMalformed  = errors.New("invalid Muesli transfer JSON")
	ErrRemoteTooLarge   = errors.New("muesli transfer exceeds 16 MiB")
)

// RemoteRequest transfers selected meeting evidence, never a database or path.
type RemoteRequest struct {
	Action       string               `json:"action" enum:"register,upsert,refresh"`
	Source       meetingimport.Source `json:"source"`
	Meeting      *RemoteMeeting       `json:"meeting,omitzero"`
	Full         bool                 `json:"full,omitempty"`
	BuildCache   bool                 `json:"build_cache,omitempty"`
	NoBuildCache bool                 `json:"no_build_cache,omitempty"`
}

type RemoteMeeting struct {
	Record        rawMeeting          `json:"record"`
	Participants  []RemoteParticipant `json:"participants,omitempty" maxItems:"200"`
	ContactsState ContactsState       `json:"contacts_state" enum:"complete,partial,unavailable,off"`
}

// RemoteParticipant deliberately has no local Contacts identifier field.
type RemoteParticipant struct {
	Ref                   string   `json:"ref,omitempty" maxLength:"16"`
	Name                  string   `json:"name,omitempty" maxLength:"1024"`
	Email                 string   `json:"email,omitempty"`
	Source                string   `json:"source,omitempty"`
	Emails                []string `json:"emails,omitempty" maxItems:"50"`
	Phones                []string `json:"phones,omitempty" maxItems:"50"`
	LinkExcludedAddresses []string `json:"link_excluded_addresses,omitempty" maxItems:"50"`
	Anchor                string   `json:"anchor,omitempty" maxLength:"78"`
	Resolution            string   `json:"resolution,omitempty" enum:",resolved,carried_forward"`
	SkippedPhones         int      `json:"skipped_phones,omitempty"`
}

type RemoteResult struct {
	Status    string `json:"status" enum:"registered,created,updated,unchanged,refreshed"`
	SourceID  int64  `json:"source_id"`
	MessageID int64  `json:"message_id,omitempty"`
	Changed   bool   `json:"changed"`
}

func NewRemoteMeeting(m Meeting) *RemoteMeeting {
	created := m.CreatedAt
	if parsed, err := parseCreatedAt(created); err == nil {
		created = parsed.Format("2006-01-02T15:04:05Z")
	}
	r := &RemoteMeeting{ContactsState: m.ContactsState, Record: rawMeeting{
		ID: m.ID, Title: m.Title, StartTime: m.StartTime, EndTime: m.EndTime, CreatedAt: created,
		DurationSeconds: m.DurationSeconds, Status: m.Status, Source: m.Source,
		RawTranscript: m.RawTranscript, FormattedNotes: m.FormattedNotes, NotesState: NotesState(m.FormattedNotes), ManualNotes: m.ManualNotes,
		WordCount: m.WordCount, TemplateName: m.TemplateName, TemplateKind: m.TemplateKind,
		CalendarEventID: m.CalendarEventID, CalendarSource: m.CalendarSource, CalendarSeriesID: m.CalendarSeriesID,
		Folder: m.Folder, FollowUpToID: m.FollowUpToID,
	}}
	if r.ContactsState == "" {
		r.ContactsState = ContactsOff
	}
	for _, p := range m.Participants {
		r.Participants = append(r.Participants, RemoteParticipant{Ref: p.stableRef(), Name: p.Name, Email: p.Email, Source: p.Source,
			Emails: slices.Clone(p.ContactEmails), Phones: slices.Clone(p.ContactPhones), LinkExcludedAddresses: slices.Clone(p.LinkExcludedAddresses),
			Anchor: p.Anchor, Resolution: p.Resolution, SkippedPhones: p.SkippedPhones})
	}
	return r
}

func (r RemoteMeeting) toMeeting() Meeting {
	v := r.Record
	m := Meeting{ID: v.ID, Title: v.Title, StartTime: v.StartTime, EndTime: v.EndTime, CreatedAt: v.CreatedAt,
		DurationSeconds: v.DurationSeconds, Status: v.Status, Source: v.Source, RawTranscript: v.RawTranscript,
		FormattedNotes: v.FormattedNotes, ManualNotes: v.ManualNotes, WordCount: v.WordCount, TemplateName: v.TemplateName, TemplateKind: v.TemplateKind,
		CalendarEventID: v.CalendarEventID, CalendarSource: v.CalendarSource, CalendarSeriesID: v.CalendarSeriesID, Folder: v.Folder, FollowUpToID: v.FollowUpToID, ContactsState: r.ContactsState}
	for _, p := range r.Participants {
		m.Participants = append(m.Participants, Participant{ref: p.Ref, Name: p.Name, Email: p.Email, Source: p.Source,
			ContactEmails: slices.Clone(p.Emails), ContactPhones: slices.Clone(p.Phones), LinkExcludedAddresses: slices.Clone(p.LinkExcludedAddresses),
			Anchor: p.Anchor, Resolution: p.Resolution, SkippedPhones: p.SkippedPhones})
	}
	return m
}

func remoteInvalid(field, rule string) error {
	return fmt.Errorf("%w: %s %s", ErrRemoteValidation, field, rule)
}

// DecodeRemoteRequest applies the meeting import JSON and byte-bound rules,
// then validates the provider contract. Parse errors never echo data.
func DecodeRemoteRequest(reader io.Reader, limit int64) (RemoteRequest, error) {
	if limit <= 0 {
		return RemoteRequest{}, ErrRemoteTooLarge
	}
	if limit > MaxRemoteRequestBytes {
		limit = MaxRemoteRequestBytes
	}
	if reader == nil {
		return RemoteRequest{}, ErrRemoteMalformed
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return RemoteRequest{}, ErrRemoteMalformed
	}
	if int64(len(data)) > limit {
		return RemoteRequest{}, ErrRemoteTooLarge
	}
	var r RemoteRequest
	if !utf8.Valid(data) || json.Unmarshal(data, &r, json.RejectUnknownMembers(true)) != nil {
		return RemoteRequest{}, ErrRemoteMalformed
	}
	return r.Normalize()
}

func (r RemoteRequest) Normalize() (RemoteRequest, error) {
	if r.BuildCache && r.NoBuildCache {
		return r, remoteInvalid("cache flags", "are mutually exclusive")
	}
	if r.BuildCache && r.Action != "refresh" {
		return r, remoteInvalid("build_cache", "requires the refresh action")
	}
	// Reuse the provider-neutral source/email contract without importing content.
	probe := meetingimport.Request{Source: r.Source, Meeting: meetingimport.Meeting{ExternalID: "validation", StartedAt: "2000-01-01T00:00:00Z", Transcript: "validation"}}
	normal, err := probe.Normalize()
	if err != nil {
		return r, remoteInvalid("source", "requires a bounded identifier and valid account email")
	}
	r.Source = normal.Source
	if strings.IndexFunc(r.Source.Identifier, unicode.IsControl) >= 0 {
		return r, remoteInvalid("source.identifier", "must not contain control characters")
	}
	switch r.Action {
	case "register", "refresh":
		if r.Meeting != nil || r.Full {
			return r, remoteInvalid("meeting", "must be omitted for this action")
		}
		return r, nil
	case "upsert":
		if r.Meeting == nil {
			return r, remoteInvalid("meeting", "is required")
		}
	default:
		return r, remoteInvalid("action", "must be register, upsert or refresh")
	}
	m := *r.Meeting
	m.Participants = slices.Clone(m.Participants)
	r.Meeting = &m
	v := m.Record
	if v.ID <= 0 || v.WordCount < 0 || v.FollowUpToID < 0 {
		return r, remoteInvalid("meeting.record", "requires nonnegative counters and a positive id")
	}
	if v.Status != "completed" || m.toMeeting().Eligibility() != "" {
		return r, remoteInvalid("meeting.record", "must be a completed meeting with content")
	}
	if math.IsNaN(v.DurationSeconds) || math.IsInf(v.DurationSeconds, 0) || v.DurationSeconds < 0 || v.DurationSeconds >= float64(math.MaxInt64)/float64(time.Second) {
		return r, remoteInvalid("meeting.record.duration_seconds", "must be finite, nonnegative and fit a duration")
	}
	if v.EndTime != "" {
		started, startErr := time.Parse(time.RFC3339Nano, v.StartTime)
		ended, endErr := time.Parse(time.RFC3339Nano, v.EndTime)
		if startErr != nil || endErr != nil || ended.Before(started) {
			return r, remoteInvalid("meeting.record.end_time", "must be a timestamp on or after start_time")
		}
	}
	if _, err := m.toMeeting().ArchiveSnapshot(0, r.Source.Identifier, r.Source.AccountEmail); err != nil {
		return r, remoteInvalid("meeting.record", "requires valid creation and start timestamps")
	}
	if v.NotesState != NotesState(v.FormattedNotes) {
		return r, remoteInvalid("meeting.record.notes_state", "must match formatted notes")
	}
	for _, value := range []struct {
		field, value string
		limit        int
	}{
		{"title", v.Title, 4096}, {"start_time", v.StartTime, 128}, {"end_time", v.EndTime, 128}, {"created_at", v.CreatedAt, 128},
		{"source", v.Source, 128}, {"template_name", v.TemplateName, 4096}, {"template_kind", v.TemplateKind, 128},
		{"calendar_event_id", v.CalendarEventID, 4096}, {"calendar_source", v.CalendarSource, 128}, {"calendar_series_id", v.CalendarSeriesID, 4096}, {"folder", v.Folder, 4096},
	} {
		if !utf8.ValidString(value.value) || utf8.RuneCountInString(value.value) > value.limit {
			return r, remoteInvalid("meeting.record."+value.field, "exceeds its text limit")
		}
	}
	switch m.ContactsState {
	case ContactsComplete, ContactsPartial, ContactsUnavailable, ContactsOff:
	default:
		return r, remoteInvalid("contacts_state", "is invalid")
	}
	if len(m.Participants) > 200 {
		return r, remoteInvalid("participants", "must contain at most 200 entries")
	}
	for i := range m.Participants {
		p := &m.Participants[i]
		if !utf8.ValidString(p.Name) || !utf8.ValidString(p.Source) || utf8.RuneCountInString(p.Name) > 1024 || utf8.RuneCountInString(p.Source) > 128 || p.SkippedPhones < 0 {
			return r, remoteInvalid("participant", "exceeds its field limits")
		}
		if p.Ref != "" && !remoteHex(p.Ref, 16) {
			return r, remoteInvalid("participant.ref", "must be a hashed identifier")
		}
		identitySet := make(map[string]bool)
		for _, identity := range append(append(slices.Clone(p.Emails), p.Phones...), p.Email) {
			if identity != "" {
				identitySet[identity] = true
			}
		}
		if len(identitySet) > 50 || len(p.Emails)+len(p.Phones) > 50 || len(p.LinkExcludedAddresses) > 50 {
			return r, remoteInvalid("participant identities", "must contain at most 50 entries")
		}
		if p.Anchor != "" && (m.ContactsState != ContactsComplete || p.Resolution != resolutionResolved || !strings.HasPrefix(p.Anchor, "apple-contact:") || !remoteHex(strings.TrimPrefix(p.Anchor, "apple-contact:"), 64)) {
			return r, remoteInvalid("participant.anchor", "requires complete resolved Contacts evidence and a hashed anchor")
		}
		if p.Resolution != "" && p.Resolution != resolutionResolved && p.Resolution != resolutionCarried {
			return r, remoteInvalid("participant.resolution", "is invalid")
		}
		if m.ContactsState == ContactsOff && (p.Resolution != "" || len(p.Emails)+len(p.Phones)+len(p.LinkExcludedAddresses) > 0 || p.SkippedPhones > 0) {
			return r, remoteInvalid("participant", "must omit Contacts enrichment when disabled")
		}
		if p.Resolution == "" && (len(p.Emails)+len(p.Phones)+len(p.LinkExcludedAddresses) > 0) {
			return r, remoteInvalid("participant", "requires a resolution for Contacts enrichment")
		}
		if m.ContactsState == ContactsUnavailable && p.Resolution == resolutionResolved {
			return r, remoteInvalid("participant.resolution", "cannot be resolved when Contacts is unavailable")
		}
		identities := []meetingimport.MeetingPerson{}
		if p.Email != "" {
			identities = append(identities, meetingimport.MeetingPerson{Email: p.Email})
		}
		for _, email := range p.Emails {
			identities = append(identities, meetingimport.MeetingPerson{Email: email})
		}
		for _, phone := range p.Phones {
			identities = append(identities, meetingimport.MeetingPerson{Phone: phone})
		}
		for _, excluded := range p.LinkExcludedAddresses {
			if excluded != p.Email && !slices.Contains(p.Emails, excluded) && !slices.Contains(p.Phones, excluded) {
				return r, remoteInvalid("participant exclusions", "must reference an included identity")
			}
		}
		probe.Meeting.Attendees = identities
		if _, err := probe.Normalize(); err != nil {
			return r, remoteInvalid("participant identities", "must be valid emails or E.164 phones")
		}
		for _, email := range append([]string{p.Email}, p.Emails...) {
			if email != strings.ToLower(strings.TrimSpace(email)) {
				return r, remoteInvalid("participant email", "must be normalized")
			}
		}
		for _, phone := range p.Phones {
			normalized, ok := NormalizeContactPhone(phone, "")
			if !ok || normalized != phone {
				return r, remoteInvalid("participant phone", "must be E.164")
			}
		}
	}
	return r, nil
}

func remoteHex(value string, size int) bool {
	if len(value) != size || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// ImportRemote commits a bounded meeting under the daemon's operation gate.
// A muesli_push history entry records this one meeting, not a completed scan.
func (imp *Importer) ImportRemote(ctx context.Context, r RemoteRequest) (result RemoteResult, retErr error) {
	if imp == nil || imp.store == nil {
		return result, meetingarchive.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	r, err := r.Normalize()
	if err != nil {
		return result, err
	}
	var source *store.Source
	if r.Action == "register" {
		source, err = imp.store.GetOrCreateSource(SourceType, r.Source.Identifier)
	} else {
		source, err = imp.store.GetSourceByTypeAndIdentifier(SourceType, r.Source.Identifier)
	}
	if err != nil {
		return result, err
	}
	result.SourceID = source.ID
	identities, err := imp.store.ListAccountIdentitiesContext(ctx, source.ID)
	if err != nil {
		return result, err
	}
	hasRecorder, matches := false, false
	for _, identity := range identities {
		if slices.Contains(strings.Split(identity.SourceSignal, ","), "account-email") {
			hasRecorder = true
			matches = matches || store.EqualIdentifier(identity.Address, r.Source.AccountEmail)
		}
	}
	if hasRecorder && !matches {
		return result, remoteInvalid("source.account_email", "does not match the registered recorder")
	}
	if r.Action == "register" {
		if err := imp.store.AddAccountIdentityContext(ctx, source.ID, r.Source.AccountEmail, "account-email"); err != nil {
			return result, err
		}
		if err := imp.store.UpdateSourceDisplayNameContext(ctx, source.ID, r.Source.Identifier); err != nil {
			return result, err
		}
		result.Status = "registered"
		return result, nil
	}
	if !matches {
		return result, remoteInvalid("source", "requires registration with this recorder")
	}
	if r.Action == "refresh" {
		result.Status = "refreshed"
		return result, nil
	}
	m := r.Meeting.toMeeting()
	if m.ContactsState != ContactsOff {
		key, _ := m.SourceMessageID()
		previous, err := imp.previousParticipants(source.ID, key)
		if err != nil {
			return result, err
		}
		for i := range m.Participants {
			p := &m.Participants[i]
			if p.Resolution != "" {
				continue
			}
			if earlier, ok := previous[p.stableRef()]; ok && len(earlier.Emails)+len(earlier.Phones) > 0 {
				p.ContactEmails, p.ContactPhones, p.Resolution = earlier.Emails, earlier.Phones, resolutionCarried
			}
		}
	}
	snapshot, err := m.ArchiveSnapshot(source.ID, r.Source.Identifier, r.Source.AccountEmail)
	if err != nil {
		return result, remoteInvalid("meeting", "cannot be normalized")
	}
	syncID, err := imp.store.StartSyncContext(ctx, source.ID, "muesli_push")
	if err != nil {
		return result, err
	}
	scoped := imp.store.ScopedToSync(source.ID, syncID)
	checkpoint := &store.Checkpoint{MessagesProcessed: 1}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, scoped.FailSyncWithCheckpoint(syncID, "Muesli remote import failed", checkpoint))
		}
	}()
	archived, err := meetingarchive.New(scoped).Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: r.Full})
	result.MessageID, result.Changed = archived.MessageID, archived.Changed || archived.Links.Linked > 0
	result.Status = "unchanged"
	if archived.Created {
		result.Status = "created"
		checkpoint.MessagesAdded = 1
	} else if result.Changed {
		result.Status = "updated"
		checkpoint.MessagesUpdated = 1
	}
	if err != nil {
		return result, err
	}
	if err := scoped.UpdateSyncCheckpointContext(ctx, syncID, checkpoint); err != nil {
		return result, err
	}
	if err := scoped.CompleteSyncContext(ctx, syncID, ""); err != nil {
		return result, err
	}
	return result, nil
}
