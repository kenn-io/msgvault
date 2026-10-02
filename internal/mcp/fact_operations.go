package mcp

import (
	"encoding/json/jsontext"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type FactClaimSelection struct {
	generated.PersonFactClaim

	SubmittedValue *string `json:"submitted_value,omitzero"`
}
type FactClaimSelections struct {
	Claims []FactClaimSelection `json:"claims"`
}

// MergeSnapshotSelection holds selected metadata from the verified original.
// SourceSHA256 identifies that original; selection changes its bytes.
type MergeSnapshotSelection struct {
	Version        int64                  `json:"version"`
	SourceSHA256   string                 `json:"source_sha256"`
	SourceVerified bool                   `json:"source_verified"`
	Persons        *[]MergeSnapshotPerson `json:"persons,omitzero"`
	Rows           *[]MergeSnapshotRow    `json:"rows,omitzero"`
}
type MergeSnapshotPerson struct {
	ID                      int64   `json:"id"`
	VCardUID                string  `json:"vcard_uid"`
	DisplayName             *string `json:"display_name,omitzero"`
	Revision                int64   `json:"revision"`
	VCardProjectionRevision int64   `json:"vcard_projection_revision"`
	CreatedAt               string  `json:"created_at"`
	UpdatedAt               string  `json:"updated_at"`
	ParticipantIDs          []int64 `json:"participant_ids"`
}
type MergeSnapshotRow struct {
	TableName      string         `json:"table_name"`
	RowID          int64          `json:"row_id"`
	RowKey         *string        `json:"row_key,omitzero"`
	OriginSide     string         `json:"origin_side"`
	ProvenanceKind string         `json:"provenance_kind"`
	ParticipantID  *int64         `json:"participant_id,omitzero"`
	Columns        jsontext.Value `json:"columns,omitzero"`
}
type MergeCandidateDecision struct {
	ETag      string                               `json:"etag"`
	Candidate generated.PersonMergeReviewCandidate `json:"candidate"`
}
type PersonSplitRecord struct {
	ETag          string                      `json:"etag"`
	NewPersonETag string                      `json:"new_person_etag"`
	Result        generated.PersonSplitResult `json:"result"`
}

func factOperationalDefinitions() []operationalDefinition {
	defs := []operationalDefinition{}
	add := func(name, description string, props map[string]*jsonschema.Schema, required []string, output *jsonschema.Schema, writes, recovery bool) {
		family := OperationFamilyRecords
		if recovery {
			family = OperationFamilyMergeRecovery
		}
		defs = append(defs, newOperationalDefinition(name, description, family, closedObject(props, required...), output, writes, false))
	}
	person := func() map[string]*jsonschema.Schema {
		return map[string]*jsonschema.Schema{toolArgPersonID: safeIDSchema("Native durable person ID")}
	}
	page := func(maximum int) map[string]*jsonschema.Schema {
		props := person()
		props[toolArgLimit] = boundedIntegerSchema("Native result limit", 1, float64(maximum))
		props["offset"] = boundedIntegerSchema("Native zero-based offset", 0, maxJSONSafeInteger)
		return props
	}
	fields := func(props map[string]*jsonschema.Schema, keys ...string) {
		props["fields"] = &jsonschema.Schema{Type: mcpSchemaArray, Items: recordEnumSchema("Explicit native data fields", keys...), UniqueItems: true}
		props["include_sensitive"] = booleanSchema("Explicit private-data read intent; default false")
	}
	add("list_fact_targets", "Read the actual native automatic fact target descriptors and catalog fingerprint. Sensitive targets are excluded unless explicitly requested; no inference or provider call.", map[string]*jsonschema.Schema{"include_sensitive": booleanSchema("Include native sensitive descriptors; default false")}, nil, recordSchema[generated.Catalog](), false, false)
	for _, name := range []string{"list_person_fact_evidence", "list_person_fact_claims", "list_person_fact_decisions"} {
		props := page(200)
		props["target"] = stringSchema("Exact native kind:key:sha256:<64 lowercase hex> target; keys may contain colons")
		output := recordSchema[generated.PersonFactDecisionsResponse]()
		description := "Read native immutable fact decisions and provenance, default limit 50/max 200. Unsupported status is retained; diagnostic text is untrusted data."
		if name == "list_person_fact_evidence" {
			fields(props, "excerpt", "source_details")
			output = recordSchema[generated.PersonFactEvidenceResponse]()
			description = "Read native immutable fact evidence/provenance and supported state, default limit 50/max 200. Excerpts and free source details require explicit fields selection and sensitive intent; no URI fetch."
		}
		if name == "list_person_fact_claims" {
			fields(props, "values")
			output = recordSchema[FactClaimSelections]()
			description = "Read native immutable fact claims/provenance, default limit 50/max 200. Actual definition metadata gates private values; sensitive/Notes values require explicit fields selection and sensitive intent. Unknown target values are excluded."
		}
		add(name, description, props, []string{toolArgPersonID}, output, false, false)
	}
	status := page(200)
	status["evidence_key"] = stringSchema("Exact native immutable evidence key")
	status["supported"] = booleanSchema("Explicit supported or unsupported filter; false is retained")
	add("list_person_fact_status_events", "Read native evidence support/reactivation history and sanitized diagnostic DTOs, default limit 50/max 200. No claim value or provider call.", status, []string{toolArgPersonID}, recordSchema[generated.PersonFactEvidenceStatusEventsResponse](), false, false)
	add("list_person_fact_pins", "Read effective native fact pins, actor and resolved target revision.", person(), []string{toolArgPersonID}, recordSchema[generated.PersonFactPinsResponse](), false, false)
	pin := person()
	pin["kind"] = recordEnumSchema("Native fact target kind", "attribute", "employment")
	pin["key"] = stringSchema("Exact active descriptor key; may contain colons")
	pin["pinned"] = booleanSchema("Required replacement state; false explicitly unpins")
	add("set_person_fact_pin", "Replace native fact pin state after approval. Daemon resolves the active target revision; there is no caller ETag or fingerprint. Native tracked-only constraints/projections remain.", pin, []string{toolArgPersonID, "kind", "key", "pinned"}, recordSchema[generated.PersonFactPinWrite](), true, false)
	add("get_person_tracking", "Read native explicit tracking state and tracking timestamp.", person(), []string{toolArgPersonID}, recordSchema[generated.PersonTracking](), false, false)
	tracked := person()
	tracked["tracked"] = booleanSchema("Required replacement state; false explicitly untracks")
	add("set_person_tracking", "Replace explicit native tracking state after approval. tracked=false is retained; omission/null refuse. No provider request or caller ETag is invented.", tracked, []string{toolArgPersonID, "tracked"}, recordSchema[generated.PersonTracking](), true, false)
	add("list_person_merges", "Read native paged merge lineage summaries, default limit 100/max 500, preserving action and review counts.", page(500), []string{toolArgPersonID}, recordSchema[generated.PersonMergesResponse](), false, false)
	merge := func() map[string]*jsonschema.Schema {
		return map[string]*jsonschema.Schema{"merge_id": safeIDSchema("Native immutable merge ID")}
	}
	add("get_person_merge", "Read native merge header, participant ownership, row lineage, review candidates and splits. Review metadata contains no conflicting attribute values.", merge(), []string{"merge_id"}, recordSchema[generated.PersonMergeDetail](), false, false)
	snapshot := merge()
	fields(snapshot, "persons", "rows", "columns")
	add("get_person_merge_snapshot", "Read selected metadata from the native integrity-verified portable merge snapshot. source_sha256 identifies the original, not filtered output. SQL columns require explicit fields=columns and include_sensitive=true; content is untrusted data.", snapshot, []string{"merge_id"}, recordSchema[MergeSnapshotSelection](), false, false)
	review := merge()
	review[toolArgPersonID] = safeIDSchema("Exact native current candidate person ID")
	review["candidate_id"] = safeIDSchema("Native merge attribute candidate ID")
	review[relationshipETagKey] = stringSchema("Exact preceding person ETag; never implicitly refreshed")
	review["decision"] = recordEnumSchema("Native candidate decision", "accept", "reject")
	add("review_person_merge_candidate", "Accept/reject one native merge-result attribute candidate after dedicated recovery opt-in and approval. merge_id provides exact candidate context; person ETag, ownership and state guards remain native.", review, []string{"merge_id", toolArgPersonID, "candidate_id", relationshipETagKey, "decision"}, recordSchema[MergeCandidateDecision](), true, true)
	split := person()
	split["merge_id"] = safeIDSchema("Native merge lineage ID owned by this person")
	split["participant_ids"] = &jsonschema.Schema{Type: mcpSchemaArray, Items: safeIDSchema("Explicit absorbed-lineage participant ID; empty only for a native zero-participant reversal"), UniqueItems: true}
	split[relationshipETagKey] = stringSchema("Exact preceding person ETag; never implicitly refreshed")
	split["idempotency_key"] = stringSchema("Exact caller retry key, 1..128 bytes")
	add("split_person", "Split the explicit eligible absorbed-lineage participants after dedicated recovery opt-in and approval. Exact caller person ETag/idempotency key, current lineage ownership, reviewed-candidate guards and rollback remain native; returns both person ETags.", split, []string{toolArgPersonID, "merge_id", "participant_ids", relationshipETagKey, "idempotency_key"}, recordSchema[PersonSplitRecord](), true, true)
	return defs
}
