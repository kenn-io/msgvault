package hex

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/store"
)

const MaxRemoteRequestBytes = meetingimport.MaxRequestBytes

var ErrRemoteValidation = errors.New("Hex transfer failed validation")
var ErrRemoteRecordValidation = fmt.Errorf("%w: meeting failed validation", ErrRemoteValidation)
var ErrRemoteMalformed = errors.New("invalid Hex transfer JSON")
var ErrRemoteTooLarge = errors.New("Hex transfer exceeds 16 MiB")

type RemoteRequest struct {
	Action       string               `json:"action" enum:"register,upsert,refresh"`
	Source       meetingimport.Source `json:"source"`
	Meeting      *Meeting             `json:"meeting,omitzero"`
	Full         bool                 `json:"full,omitempty"`
	BuildCache   bool                 `json:"build_cache,omitempty"`
	NoBuildCache bool                 `json:"no_build_cache,omitempty"`
}
type RemoteResult struct {
	Status    string `json:"status" enum:"registered,created,updated,unchanged,refreshed"`
	SourceID  int64  `json:"source_id"`
	MessageID int64  `json:"message_id,omitempty"`
	Changed   bool   `json:"changed"`
}

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
		return r, ErrRemoteMalformed
	}
	return r.Normalize()
}
func (r RemoteRequest) Normalize() (RemoteRequest, error) {
	source, err := meetingimport.NormalizeRecorderSource(r.Source)
	if err != nil {
		return r, ErrRemoteValidation
	}
	r.Source = source
	if r.BuildCache && (r.NoBuildCache || r.Action != "refresh") {
		return r, ErrRemoteValidation
	}
	switch r.Action {
	case "register", "refresh":
		if r.Meeting != nil || r.Full {
			return r, ErrRemoteValidation
		}
		return r, nil
	case "upsert":
		if r.Meeting == nil {
			return r, ErrRemoteRecordValidation
		}
	default:
		return r, ErrRemoteValidation
	}
	m := r.Meeting
	v := m.Manifest
	if !validID(v.ID) || len(v.ID) > 256 || v.SchemaVersion != 1 || v.CreatedAtMS <= 0 || v.CreatedAtMS > 253402300799999 || len(v.Title) > 16384 || len(v.App) > 1024 {
		return r, ErrRemoteRecordValidation
	}
	if v.EndedAtMS != nil && (*v.EndedAtMS < v.CreatedAtMS || *v.EndedAtMS > 253402300799999) {
		return r, ErrRemoteRecordValidation
	}
	if v.DurationMS != nil && (*v.DurationMS < 0 || *v.DurationMS > 9223372036854) {
		return r, ErrRemoteRecordValidation
	}
	switch v.Status {
	case "complete", "interrupted", "failed":
	default:
		return r, ErrRemoteRecordValidation
	}
	switch m.TranscriptState {
	case "final", "rolling", "missing", "unreadable":
	default:
		return r, ErrRemoteRecordValidation
	}
	if v.Calendar != nil && len(v.Calendar.Attendees) > 200 {
		return r, ErrRemoteRecordValidation
	}
	for _, e := range m.Entries {
		if !validEntry(e) || e.EndMS > 9223372036854 {
			return r, ErrRemoteRecordValidation
		}
	}
	for _, c := range v.Chapters {
		if c.StartMS < 0 || c.StartMS > 9223372036854 {
			return r, ErrRemoteRecordValidation
		}
	}
	return r, nil
}
func (imp *Importer) registeredSource(ctx context.Context, source meetingimport.Source, register bool) (*store.Source, error) {
	var st *store.Source
	var err error
	if register {
		st, err = imp.store.GetOrCreateSource(SourceType, source.Identifier)
	} else {
		st, err = imp.store.GetSourceByTypeAndIdentifier(SourceType, source.Identifier)
	}
	if err != nil {
		return nil, err
	}
	identities, err := imp.store.ListAccountIdentitiesContext(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	has, matches := false, false
	for _, identity := range identities {
		if slices.Contains(strings.Split(identity.SourceSignal, ","), "account-email") {
			has = true
			matches = matches || store.EqualIdentifier(identity.Address, source.AccountEmail)
		}
	}
	if (has && !matches) || (!register && !matches) {
		return nil, ErrRemoteValidation
	}
	if register {
		if err := imp.store.AddAccountIdentityContext(ctx, st.ID, source.AccountEmail, "account-email"); err != nil {
			return nil, err
		}
		if source.DisplayName != "" {
			if err := imp.store.UpdateSourceDisplayNameContext(ctx, st.ID, source.DisplayName); err != nil {
				return nil, err
			}
		}
	}
	return st, nil
}
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
	source, err := imp.registeredSource(ctx, r.Source, r.Action == "register")
	if err != nil {
		return result, err
	}
	result.SourceID = source.ID
	if r.Action == "register" {
		result.Status = "registered"
		return result, nil
	}
	if r.Action == "refresh" {
		result.Status = "refreshed"
		return result, nil
	}
	snapshot, err := r.Meeting.ArchiveSnapshot(source.ID, r.Source.Identifier, r.Source.AccountEmail)
	if err != nil {
		return result, ErrRemoteRecordValidation
	}
	archiver := meetingarchive.New(imp.store)
	if !r.Full {
		unchanged, err := archiver.Unchanged(ctx, snapshot)
		if err != nil {
			return result, err
		}
		if unchanged {
			archived, err := archiver.Upsert(ctx, snapshot, meetingarchive.UpsertOptions{})
			result.Status = "unchanged"
			result.MessageID = archived.MessageID
			return result, err
		}
	}
	syncID, err := imp.store.StartSyncContext(ctx, source.ID, "hex_push")
	if err != nil {
		return result, err
	}
	scoped := imp.store.ScopedToSync(source.ID, syncID)
	checkpoint := &store.Checkpoint{MessagesProcessed: 1}
	defer func() {
		if retErr != nil {
			_ = scoped.FailSyncWithCheckpoint(syncID, "Hex remote import failed", checkpoint)
		}
	}()
	archived, err := meetingarchive.New(scoped).Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: r.Full})
	result.MessageID = archived.MessageID
	result.Changed = archived.Changed
	result.Status = "unchanged"
	if archived.Created {
		result.Status = "created"
		checkpoint.MessagesAdded = 1
	} else if archived.Changed {
		result.Status = "updated"
		checkpoint.MessagesUpdated = 1
	}
	if err != nil {
		return result, err
	}
	if err := scoped.UpdateSyncCheckpoint(syncID, checkpoint); err != nil {
		return result, err
	}
	return result, scoped.CompleteSync(syncID, "")
}
