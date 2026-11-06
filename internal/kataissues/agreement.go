package kataissues

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"reflect"

	"go.kenn.io/msgvault/internal/agreements"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/taskclient"
)

const AgreementMetadataKey = "msgvault.agreement"

type AgreementInput = agreements.Input
type AgreementTerms = agreements.Terms
type AgreementView = agreements.View

var (
	ErrAgreementConflict         = agreements.ErrConflict
	ErrUnsupportedAgreement      = agreements.ErrUnsupported
	ErrAgreementFull             = agreements.ErrFull
	ErrAgreementClosed           = errors.New("closed tasks cannot record new agreement events")
	ErrAgreementEvidenceUnlinked = errors.New("link every agreement citation to this task first")
)

type agreementWriter interface {
	MutateMetadata(ctx context.Context, project, taskID, revision string, patch map[string]any) (taskclient.KataTask, error)
}

// Agreement reads only saved history, including when its sources are no longer
// available. Saved quotes do not assert current archive availability.
func (s *Service) Agreement(ctx context.Context, ref string) (AgreementView, error) {
	project, ref, err := s.agreementRef(ref)
	if err != nil {
		return AgreementView{}, err
	}
	task, err := s.Kata.GetTask(ctx, project, ref)
	if err != nil {
		return AgreementView{}, err
	}
	return agreementMetadata(task.Metadata)
}

func agreementMetadata(metadata map[string]any) (AgreementView, error) {
	value, present := metadata[AgreementMetadataKey]
	if present && value == nil {
		return AgreementView{}, ErrUnsupportedAgreement
	}
	return agreements.Decode(value)
}

func (s *Service) agreementRef(ref string) (string, string, error) {
	project, ref, err := s.splitRef(ref)
	if err != nil {
		return "", "", err
	}
	if project != s.Project {
		return "", "", ErrOutsideProject
	}
	return project, ref, nil
}

// RecordAgreement appends an owner-reviewed event to an existing Kata task.
// The task revision guards history, linked evidence, and open status together.
// There is no automatic conflict retry: the owner must review the newer state.
func (s *Service) RecordAgreement(ctx context.Context, ref string, input AgreementInput) (AgreementView, error) {
	input, err := agreements.Normalize(input)
	if err != nil {
		return AgreementView{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	project, ref, err := s.agreementRef(ref)
	if err != nil {
		return AgreementView{}, err
	}
	task, err := s.Kata.GetTask(ctx, project, ref)
	if err != nil {
		return AgreementView{}, err
	}
	view, err := agreementMetadata(task.Metadata)
	if err != nil {
		return AgreementView{}, err
	}
	// Recognize original input before today's count, status, membership, capacity
	// or source checks. This also recovers a successful write whose response was lost.
	for _, event := range view.History {
		if event.Input.ID == input.ID {
			if !reflect.DeepEqual(event.Input, input) {
				return AgreementView{}, ErrAgreementConflict
			}
			view.Replayed = true
			return view, nil
		}
	}
	if input.ExpectedEvents != len(view.History) {
		return AgreementView{}, ErrAgreementConflict
	}
	if task.Status != "open" {
		return AgreementView{}, ErrAgreementClosed
	}
	envelope, err := parseEnvelope(task.Metadata[EvidenceMetadataKey])
	if err != nil {
		return AgreementView{}, ErrAgreementEvidenceUnlinked
	}
	linked := map[string]bool{}
	for _, entry := range envelope.Entries {
		linked[entry.ID] = true
	}
	for _, evidence := range input.Evidence {
		if !linked[kataevidence.ID(evidence)] {
			return AgreementView{}, ErrAgreementEvidenceUnlinked
		}
	}
	quotes, err := s.resolve(ctx, input.Evidence)
	if err != nil {
		return AgreementView{}, err
	}
	snapshots := make([]string, len(quotes))
	for i, quote := range quotes {
		snapshots[i] = quote.Snapshot
	}
	agreement, result, err := agreements.Append(view.History, input, snapshots)
	if errors.Is(err, agreements.ErrInvalid) {
		return AgreementView{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if err != nil {
		return AgreementView{}, err
	}
	// Keep ample room beneath Kata's ordinary 1 MiB response limit for its wire
	// wrapper and issue fields not projected into KataTask. Include saved comments.
	preview := task
	preview.Metadata = maps.Clone(task.Metadata)
	preview.Metadata[AgreementMetadataKey] = agreement
	encoded, err := json.Marshal(preview)
	if err != nil || len(encoded) > 512<<10 {
		return AgreementView{}, ErrAgreementFull
	}
	writer, ok := s.Kata.(agreementWriter)
	if !ok {
		return AgreementView{}, taskclient.ErrIncompatible
	}
	_, err = writer.MutateMetadata(ctx, project, task.UID, task.Revision, map[string]any{AgreementMetadataKey: agreement})
	if err != nil {
		return AgreementView{}, err
	}
	return result, nil
}
