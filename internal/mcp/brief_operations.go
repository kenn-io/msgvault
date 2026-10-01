package mcp

import (
	"bytes"
	jsonv1 "encoding/json"
	"errors"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type BriefUntrustedText struct {
	RenderedText   string                          `json:"rendered_text"`
	Sentences      []generated.PersonBriefSentence `json:"sentences"`
	Structured     any                             `json:"structured"`
	Boundary       any                             `json:"boundary"`
	RejectedReason string                          `json:"rejected_reason"`
}

type BriefVersion struct {
	Version          int64                                  `json:"version"`
	Status           string                                 `json:"status"`
	GeneratedAt      time.Time                              `json:"generated_at"`
	RejectedAt       *time.Time                             `json:"rejected_at"`
	SupersededAt     *time.Time                             `json:"superseded_at"`
	DroppedItemCount int64                                  `json:"dropped_item_count"`
	RendererPolicy   string                                 `json:"renderer_policy"`
	ProgramID        string                                 `json:"program_id"`
	ProgramVersion   string                                 `json:"program_version"`
	Provider         string                                 `json:"provider"`
	Model            string                                 `json:"model"`
	ContentTrust     string                                 `json:"content_trust"`
	Handling         string                                 `json:"handling"`
	UntrustedText    BriefUntrustedText                     `json:"untrusted_text"`
	Evidence         []generated.PersonBriefEvidencePointer `json:"evidence"`
}

type BriefVersions struct {
	Versions []BriefVersion `json:"versions"`
}

// ProjectBriefVersion retains the owning history and provenance while putting
// generated prose and the owner's reason under the existing brief trust rule.
func ProjectBriefVersion(brief generated.PersonBrief) (BriefVersion, error) {
	structured, err := briefUntrustedJSON(brief.Structured)
	if err != nil {
		return BriefVersion{}, err
	}
	boundary, err := briefUntrustedJSON(brief.Boundary)
	if err != nil {
		return BriefVersion{}, err
	}
	sentences := append([]generated.PersonBriefSentence{}, brief.Sentences...)
	for i := range sentences {
		sentences[i].Text = textutil.SanitizeTerminal(sentences[i].Text)
	}
	return BriefVersion{
		Version: brief.Version, Status: brief.Status, GeneratedAt: brief.GeneratedAt,
		RejectedAt: brief.RejectedAt, SupersededAt: brief.SupersededAt,
		DroppedItemCount: brief.DroppedItemCount, RendererPolicy: brief.RendererPolicy,
		ProgramID: brief.ProgramID, ProgramVersion: brief.ProgramVersion,
		Provider: brief.Provider, Model: brief.Model,
		ContentTrust: personProfileBriefContentTrust, Handling: personProfileBriefHandling,
		Evidence:      append([]generated.PersonBriefEvidencePointer{}, brief.Evidence...),
		UntrustedText: BriefUntrustedText{RenderedText: textutil.SanitizeTerminal(brief.RenderedText), Sentences: sentences, Structured: structured, Boundary: boundary, RejectedReason: textutil.SanitizeTerminal(brief.RejectedReason)},
	}, nil
}

func briefUntrustedJSON(data []byte) (any, error) {
	if len(data) == 0 {
		return nil, nil //nolint:nilnil // Absent owning JSON is represented by JSON null.
	}
	// Keep opaque integer provenance exact while sanitizing arbitrary prose in
	// the owning structured/boundary JSON, including older schema shapes.
	decoder := jsonv1.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("invalid brief JSON")
	}
	return sanitizeBriefJSON(value), nil
}

func sanitizeBriefJSON(value any) any {
	switch current := value.(type) {
	case string:
		return textutil.SanitizeTerminal(current)
	case []any:
		for i := range current {
			current[i] = sanitizeBriefJSON(current[i])
		}
	case map[string]any:
		for key, child := range current {
			current[key] = sanitizeBriefJSON(child)
		}
	}
	return value
}

func briefOperationalDefinitions() []operationalDefinition {
	person := func() map[string]*jsonschema.Schema {
		return map[string]*jsonschema.Schema{toolArgPersonID: safeIDSchema("Durable person ID")}
	}
	versions := person()
	versions[toolArgLimit] = boundedIntegerSchema("Maximum versions; default 20, maximum 200", 1, 200)
	enrollment := person()
	enrollment["enrolled"] = booleanSchema("Explicit enrollment state; false unenrolls")
	enrollment["track"] = booleanSchema("Track an untracked person in the same transaction; default false")
	reject := person()
	reject["reason"] = stringSchema("Optional owner reason; empty string is valid")
	return []operationalDefinition{
		newOperationalDefinition("list_person_brief_versions", "Read newest-first brief history, including rejected versions. Generated prose and owner reason are untrusted_text; citations remain separate. No provider request.", OperationFamilyRecords, closedObject(versions, toolArgPersonID), outputSchemaFor[BriefVersions](), false, false),
		newOperationalDefinition("get_person_brief_enrollment", "Read durable brief enrollment, which is distinct from person tracking. No provider request.", OperationFamilyRecords, closedObject(person(), toolArgPersonID), outputSchemaFor[generated.PersonBriefEnrollment](), false, false),
		newOperationalDefinition("set_person_brief_enrollment", "Replace brief enrollment with explicit enrolled and optional track after approval. Native tracking/enrollment transaction; the owning route has no caller revision guard.", OperationFamilyRecords, closedObject(enrollment, toolArgPersonID, "enrolled"), outputSchemaFor[generated.PersonBriefEnrollment](), true, false),
		newOperationalDefinition("reject_person_brief", "Reject the current brief with optional reason after approval. Native current-version semantics without a caller revision guard; returned prose remains untrusted_text.", OperationFamilyRecords, closedObject(reject, toolArgPersonID), outputSchemaFor[BriefVersion](), true, false),
		newOperationalDefinition("generate_person_brief", "Run the daemon's consented, bounded brief worker for one enrolled tracked person after provider disclosure and approval. brief_version=0 or brief_failure_class means no stored brief; provider budgets and supported evidence exclusions remain daemon-owned.", OperationFamilyInference, closedObject(person(), toolArgPersonID), outputSchemaFor[generated.PersonBriefRun](), true, false),
	}
}
