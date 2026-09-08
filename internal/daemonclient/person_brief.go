package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/peoplesweep"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var (
	_ peoplebrowser.PersonBriefReader = (*PeopleBrowser)(nil)
	_ peoplebrowser.PersonBriefWriter = (*PeopleBrowser)(nil)
)

// peopleBrowserWithoutBriefs serves every People operation but hides the brief
// surfaces. It embeds the Backend interface rather than *PeopleBrowser, so the
// brief methods are not promoted and a caller's PersonBriefReader assertion
// fails. A daemon that predates the brief routes therefore makes the brief
// disappear instead of reporting a failed read on every contact.
type peopleBrowserWithoutBriefs struct {
	peoplebrowser.Backend
}

// NewPeopleBrowserWithoutBriefs wraps a People browser for a daemon whose API
// schema predates the person brief routes.
//
// The embedded field is the Backend interface, so this drops *every* optional
// interface *PeopleBrowser implements, not only the brief pair. The brief
// reader and writer are the only optional interfaces asserted on a people
// backend today; the next one has to be forwarded here explicitly or it
// disappears from every daemon-backed surface without a word.
func NewPeopleBrowserWithoutBriefs(browser *PeopleBrowser) peoplebrowser.Backend {
	return peopleBrowserWithoutBriefs{Backend: browser}
}

// personBriefNotFoundCode is the daemon's answer for a person with no current
// brief version. It is a missing optional resource, not a failed read.
const personBriefNotFoundCode = "person_brief_not_found"

// GetPersonBrief reads one person's current brief version. A person who has no
// version yet returns a nil brief and no error, so the profile and the People
// browser render without one. Every other daemon failure propagates.
func (b *PeopleBrowser) GetPersonBrief(
	ctx context.Context, personID int64,
) (*peoplebrowser.PersonBrief, error) {
	if personID < 1 {
		return nil, errors.New("person ID must be positive")
	}
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.GetPersonBriefResp, error) {
			return client.GetPersonBriefWithResponse(ctx,
				&generated.GetPersonBriefRequestOptions{
					PathParams: &generated.GetPersonBriefPath{ID: personID},
				})
		})
	if err != nil {
		if absentPersonBrief(err) {
			return nil, nil //nolint:nilnil // a person with no current version has no brief and no failure
		}
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("get person brief: empty response")
	}
	return personBriefFromGenerated(*resp.JSON200), nil
}

// SetPersonBriefEnrollment replaces a person's brief enrollment. Track adds the
// tracking row enrollment requires in the same daemon transaction; without it
// an untracked person is refused.
func (b *PeopleBrowser) SetPersonBriefEnrollment(
	ctx context.Context, personID int64, enrolled, track bool,
) (*peoplebrowser.PersonBriefEnrollment, error) {
	if personID < 1 {
		return nil, errors.New("person ID must be positive")
	}
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.SetPersonBriefEnrollmentResp, error) {
			return client.SetPersonBriefEnrollmentWithResponse(ctx,
				&generated.SetPersonBriefEnrollmentRequestOptions{
					PathParams: &generated.SetPersonBriefEnrollmentPath{ID: personID},
					Body:       &generated.SetPersonBriefEnrollmentBody{Enrolled: enrolled, Track: &track},
				})
		})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("set person brief enrollment: empty response")
	}
	return &peoplebrowser.PersonBriefEnrollment{
		PersonID: resp.JSON200.PersonID, Enrolled: resp.JSON200.Enrolled,
		EnabledAt: copyTime(resp.JSON200.EnabledAt), Actor: resp.JSON200.Actor,
	}, nil
}

// GeneratePersonBrief runs one manual, forced brief attempt for a person. It
// spends provider budget for every call it makes and answers only when the
// attempt has finished.
func (b *PeopleBrowser) GeneratePersonBrief(
	ctx context.Context, personID int64,
) (*peoplebrowser.PersonBriefRun, error) {
	if personID < 1 {
		return nil, errors.New("person ID must be positive")
	}
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.GeneratePersonBriefResp, error) {
			return client.GeneratePersonBriefWithResponse(ctx,
				&generated.GeneratePersonBriefRequestOptions{
					PathParams: &generated.GeneratePersonBriefPath{ID: personID},
				})
		})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("generate person brief: empty response")
	}
	return &peoplebrowser.PersonBriefRun{
		RunID: resp.JSON200.RunID, AttemptID: resp.JSON200.AttemptID,
		BriefVersion:      int(resp.JSON200.BriefVersion),
		BriefFailureClass: resp.JSON200.BriefFailureClass,
	}, nil
}

// RejectPersonBrief records the owner's verdict on the current version and
// returns the rejected version. The reason may be empty.
func (b *PeopleBrowser) RejectPersonBrief(
	ctx context.Context, personID int64, reason string,
) (*peoplebrowser.PersonBrief, error) {
	if personID < 1 {
		return nil, errors.New("person ID must be positive")
	}
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.RejectPersonBriefResp, error) {
			return client.RejectPersonBriefWithResponse(ctx,
				&generated.RejectPersonBriefRequestOptions{
					PathParams: &generated.RejectPersonBriefPath{ID: personID},
					Body:       &generated.RejectPersonBriefBody{Reason: &reason},
				})
		})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("reject person brief: empty response")
	}
	return personBriefFromGenerated(*resp.JSON200), nil
}

// absentPersonBrief reports the daemon's "this person has no current version"
// answer, which is a 404 carrying the brief's own error code. A 404 with any
// other code (an unknown person, for instance) stays a failure.
func absentPersonBrief(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		apiErr.Status == http.StatusNotFound &&
		apiErr.APIErrorCode() == personBriefNotFoundCode
}

func personBriefFromGenerated(brief generated.PersonBrief) *peoplebrowser.PersonBrief {
	converted := &peoplebrowser.PersonBrief{
		Version: int(brief.Version), Status: brief.Status, GeneratedAt: brief.GeneratedAt,
		RenderedText: brief.RenderedText, DroppedItemCount: int(brief.DroppedItemCount),
		RejectedAt: copyTime(brief.RejectedAt), RejectedReason: brief.RejectedReason,
	}
	converted.Evidence = make([]peoplebrowser.PersonBriefEvidence, 0, len(brief.Evidence))
	for _, pointer := range brief.Evidence {
		converted.Evidence = append(converted.Evidence, personBriefEvidenceFromGenerated(pointer))
	}
	converted.Sentences = make([]peoplebrowser.PersonBriefSentence, 0, len(brief.Sentences))
	for _, sentence := range brief.Sentences {
		ordinals := make([]int, 0, len(sentence.EvidenceOrdinals))
		for _, ordinal := range sentence.EvidenceOrdinals {
			ordinals = append(ordinals, int(ordinal))
		}
		converted.Sentences = append(converted.Sentences, peoplebrowser.PersonBriefSentence{
			Kind:  peoplebrowser.PersonBriefItemKind(sentence.Kind),
			Index: int(sentence.Index), Text: sentence.Text,
			EvidenceOrdinals: ordinals,
		})
	}
	converted.Items = personBriefItems(brief)
	return converted
}

// personBriefItems projects the stored structure onto flat items in rendering
// order and attaches the archive items each one cites.
//
// The join comes from the daemon: every sentence in the response carries the
// evidence_ordinals of the pointers its own structured item cites, computed
// server-side from the parser's citation order. A sentence and an item share
// the kind and index the renderer stamped on both, so the lookup is exact.
func personBriefItems(brief generated.PersonBrief) []peoplebrowser.PersonBriefItem {
	var output peoplesweep.BriefOutput
	if err := json.Unmarshal(brief.Structured, &output); err != nil {
		return []peoplebrowser.PersonBriefItem{}
	}
	evidence := briefEvidenceByOrdinals(brief)
	items := make([]peoplebrowser.PersonBriefItem, 0,
		1+len(output.Highlights)+len(output.FollowUps)+
			len(output.Appreciations)+len(output.Uncertainties))
	if interaction := output.LastMeaningfulInteraction; interaction != nil {
		items = append(items, peoplebrowser.PersonBriefItem{
			Kind: peoplebrowser.PersonBriefLastInteraction, Index: 0,
			Text:     interaction.Summary,
			Evidence: evidence(peoplebrowser.PersonBriefLastInteraction, 0),
		})
	}
	for index, highlight := range output.Highlights {
		items = append(items, peoplebrowser.PersonBriefItem{
			Kind: peoplebrowser.PersonBriefHighlight, Index: index, Text: highlight.Text,
			Speaker:  string(highlight.Speaker),
			Evidence: evidence(peoplebrowser.PersonBriefHighlight, index),
		})
	}
	for index, followUp := range output.FollowUps {
		items = append(items, peoplebrowser.PersonBriefItem{
			Kind: peoplebrowser.PersonBriefFollowUp, Index: index, Text: followUp.Question,
			Why:      followUp.Why,
			Evidence: evidence(peoplebrowser.PersonBriefFollowUp, index),
		})
	}
	for index, appreciation := range output.Appreciations {
		items = append(items, peoplebrowser.PersonBriefItem{
			Kind: peoplebrowser.PersonBriefAppreciation, Index: index,
			Text:     appreciation.Text,
			Evidence: evidence(peoplebrowser.PersonBriefAppreciation, index),
		})
	}
	for index, uncertainty := range output.Uncertainties {
		items = append(items, peoplebrowser.PersonBriefItem{
			Kind: peoplebrowser.PersonBriefUncertainty, Index: index, Text: uncertainty.Text,
			Reason:   string(uncertainty.Kind),
			Evidence: evidence(peoplebrowser.PersonBriefUncertainty, index),
		})
	}
	return items
}

// briefEvidenceByOrdinals resolves one structured item's citations from the
// evidence_ordinals the daemon put on the matching sentence.
//
// It stays fail-closed in both directions. A response whose sentences carry no
// ordinals — an older daemon, or a version whose citation order the daemon
// could not account for — yields no per-item evidence at all, so a reader sees
// the brief's whole citation list instead of a guess. An ordinal that names no
// stored pointer is skipped rather than invented.
func briefEvidenceByOrdinals(
	brief generated.PersonBrief,
) func(kind peoplebrowser.PersonBriefItemKind, index int) []peoplebrowser.PersonBriefEvidence {
	type sentenceKey struct {
		kind  peoplebrowser.PersonBriefItemKind
		index int
	}
	ordinals := make(map[sentenceKey][]int64, len(brief.Sentences))
	for _, sentence := range brief.Sentences {
		if len(sentence.EvidenceOrdinals) == 0 {
			continue
		}
		key := sentenceKey{
			kind:  peoplebrowser.PersonBriefItemKind(sentence.Kind),
			index: int(sentence.Index),
		}
		ordinals[key] = sentence.EvidenceOrdinals
	}
	byOrdinal := make(map[int64]generated.PersonBriefEvidencePointer, len(brief.Evidence))
	for _, pointer := range brief.Evidence {
		byOrdinal[pointer.Ordinal] = pointer
	}
	return func(
		kind peoplebrowser.PersonBriefItemKind, index int,
	) []peoplebrowser.PersonBriefEvidence {
		cited := make([]peoplebrowser.PersonBriefEvidence, 0,
			len(ordinals[sentenceKey{kind: kind, index: index}]))
		for _, ordinal := range ordinals[sentenceKey{kind: kind, index: index}] {
			pointer, stored := byOrdinal[ordinal]
			if !stored {
				continue
			}
			cited = append(cited, personBriefEvidenceFromGenerated(pointer))
		}
		if len(cited) == 0 {
			return nil
		}
		return cited
	}
}

func personBriefEvidenceFromGenerated(pointer generated.PersonBriefEvidencePointer) peoplebrowser.PersonBriefEvidence {
	return peoplebrowser.PersonBriefEvidence{
		Ordinal: int(pointer.Ordinal), EvidenceID: pointer.EvidenceID,
		SourceRef: pointer.SourceRef, SourceURL: pointer.SourceURL,
		Directness: pointer.Directness, EventTime: pointer.EventTime,
		Supported: pointer.EvidenceSupported,
	}
}
