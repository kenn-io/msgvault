package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// MergeSnapshot is the verified snapshot's bounded lineage metadata, without row values.
type MergeSnapshot struct {
	Version  int64                 `json:"version"`
	SHA256   string                `json:"sha256"`
	Persons  []MergeSnapshotPerson `json:"persons"`
	RowCount int                   `json:"row_count"`
}
type MergeSnapshotPerson struct {
	ID             int64   `json:"id"`
	Revision       int64   `json:"revision"`
	ParticipantIDs []int64 `json:"participant_ids"`
}
type MergeDetail struct {
	Labels           generated.PersonMergeLabels            `json:"labels"`
	Merge            generated.PersonMerge                  `json:"merge"`
	Participants     []generated.PersonMergeParticipant     `json:"participants"`
	ReviewCandidates []generated.PersonMergeReviewCandidate `json:"review_candidates"`
	Splits           []generated.PersonSplit                `json:"splits"`
	RowCount         int                                    `json:"row_count"`
}
type MergeCandidateReceipt struct {
	Candidate generated.PersonMergeReviewCandidate `json:"candidate"`
	ETag      string                               `json:"etag"`
}

func mergeRecoveryDefinitions() []operationalDefinition {
	id := func() *jsonschema.Schema { return safeIDSchema("Durable merge ID") }
	person := func() *jsonschema.Schema { return safeIDSchema("Current durable person ID") }
	etag := func() *jsonschema.Schema {
		return stringSchema("Exact current person ETag from get_person_edit_context")
	}
	return []operationalDefinition{
		newOperationalDefinition("list_person_merges", "List one person's durable merge lineage, with bounded pagination.", OperationFamilyMergeRecovery, closedObject(map[string]*jsonschema.Schema{"person_id": person(), "limit": boundedIntegerSchema("Maximum results (default 100)", 1, 500), "offset": nonNegativeIntegerSchema("Results to skip", 0)}, "person_id"), outputSchemaFor[generated.PersonMergesResponse](), false),
		newOperationalDefinition("get_person_merge", "Read one durable merge, its participants, review candidates, row count and splits.", OperationFamilyMergeRecovery, closedObject(map[string]*jsonschema.Schema{"merge_id": id()}, "merge_id"), outputSchemaFor[MergeDetail](), false),
		newOperationalDefinition("get_person_merge_snapshot", "Verify a merge snapshot and read bounded person lineage and row count; stored row values are omitted.", OperationFamilyMergeRecovery, closedObject(map[string]*jsonschema.Schema{"merge_id": id()}, "merge_id"), outputSchemaFor[MergeSnapshot](), false),
		newOperationalDefinition("decide_person_merge_candidate", "Accept or reject one merge review candidate against the exact current person revision. Approval identifies values by ID. Requires client approval.", OperationFamilyMergeRecovery, closedObject(map[string]*jsonschema.Schema{"person_id": person(), "candidate_id": safeIDSchema("Review candidate ID"), "merge_id": id(), "etag": etag(), "decision": stringSchema("Candidate decision", "accept", "reject")}, "person_id", "merge_id", "candidate_id", "etag", "decision"), outputSchemaFor[MergeCandidateReceipt](), true),
		newOperationalDefinition("split_person_merge", "Split selected absorbed participant lineage into a new person using the exact current revision and a stable idempotency key. Native lineage and review restrictions apply. Requires client approval.", OperationFamilyMergeRecovery, closedObject(map[string]*jsonschema.Schema{"person_id": person(), "merge_id": id(), "participant_ids": {Type: schemaTypeArray, Items: safeIDSchema("Absorbed participant ID")}, "etag": etag(), "idempotency_key": stringSchema("Stable 1–128 byte retry key")}, "person_id", "merge_id", "participant_ids", "etag", "idempotency_key"), outputSchemaFor[generated.PersonSplitResult](), true),
	}
}
