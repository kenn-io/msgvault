package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"

	"go.kenn.io/msgvault/pkg/client/generated"
)

// VisualIndexStatus separates recorded progress/consent from the initialized
// authority required for approving a guarded operation.
type VisualIndexStatus struct {
	Status        generated.Status               `json:"status"`
	CurrentPolicy *generated.VisualRuntimePolicy `json:"current_policy,omitempty"`
}

type VisualRetirement struct {
	GenerationID int64 `json:"generation_id"`
	Retired      bool  `json:"retired"`
}

func visualOperationalDefinitions() []operationalDefinition {
	definitions := []operationalDefinition{
		newOperationalDefinition("get_visual_index_status", "Read visual generation progress, recorded consent and the initialized current upload policy. Coverage is an explicit rate-limited archive media scan; status never contacts the provider.", OperationFamilyVisual, closedObject(map[string]*jsonschema.Schema{"coverage": booleanSchema("Explicitly request the owning daemon's expensive media coverage scan; default false")}), outputSchemaFor[VisualIndexStatus](), false, false),
		newOperationalDefinition("get_document_vector_status", "Read document vector generation, exact consent, usage and bounded safe failures. Returns disabled or unconfigured state explicitly; never embeds documents or contacts the provider.", OperationFamilyDocuments, closedObject(map[string]*jsonschema.Schema{
			"generation_id": safeIDSchema("Exact generation whose failure diagnostics to inspect"),
			"after_token":   stringSchema("Exact failure cursor returned by the preceding response"),
			"limit":         {Type: mcpSchemaInteger, Minimum: new(float64(1)), Maximum: new(float64(1000)), Description: "Maximum failure diagnostics; default20"},
		}), outputSchemaFor[generated.DocumentVectorOperationsResponse](), false, false),
	}
	for _, name := range []string{"build_visual_index", "resume_visual_index", "retry_visual_attachment", "retire_visual_generation"} {
		properties := map[string]*jsonschema.Schema{
			"expected_generation_id":          safeIDSchema("Exact initialized generation ID from get_visual_index_status.current_policy"),
			"expected_generation_fingerprint": stringSchema("Exact initialized generation fingerprint from current_policy"),
			"expected_policy_fingerprint":     stringSchema("Exact current_policy_fingerprint; recorded consent is not current authority"),
		}
		required := []string{"expected_generation_id", "expected_generation_fingerprint", "expected_policy_fingerprint"}
		description := "Run one bounded visual pass after approval of the initialized upload scope and policy. Build records exact hosted-processing consent; resume requires existing exact consent. Media and context may leave this host and charges may apply. A changed policy refuses before execution; never automatically refreshes the guard or retries an uncertain call."
		output := outputSchemaFor[generated.Status]()
		if name == "retry_visual_attachment" {
			properties["message_id"] = safeIDSchema("Exact owning archived message ID")
			properties["blob_hash"] = stringSchema("Exact lowercase attachment SHA-256")
			required = append(required, "message_id", "blob_hash")
			description = "Retry one exact message and attachment under approved initialized policy and existing exact consent. Media/context may leave this host and charges may apply. Preserves owning scope and worker bounds; never retries an uncertain call."
		}
		if name == "retire_visual_generation" {
			properties["generation_id"] = safeIDSchema("Exact generation to retire; must equal expected_generation_id")
			required = append(required, "generation_id")
			description = "Retire the exact current visual generation and delete its backend vectors after approval. Preserves the owning daemon's generation and policy guards. Does not upload media or delete archived messages."
			output = outputSchemaFor[VisualRetirement]()
		}
		definitions = append(definitions, newOperationalDefinition(name, description, OperationFamilyVisual, closedObject(properties, required...), output, true, false))
	}
	return definitions
}
