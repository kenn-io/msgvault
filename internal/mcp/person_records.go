package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type StructuredPersonRecord struct {
	ETag    string                            `json:"etag"`
	Profile generated.StructuredPersonProfile `json:"profile"`
}
type PersonMediaContent struct {
	MediaType  string `json:"media_type"`
	DataBase64 string `json:"data_base64"`
}

func personRecordDefinitions() []operationalDefinition {
	person := func() *jsonschema.Schema { return safeIDSchema("Durable person ID") }
	patch := outputSchemaFor[generated.PersonProfilePatchRequest]()
	patch.Schema = ""
	input := closedObject(map[string]*jsonschema.Schema{"person_id": person(), "etag": stringSchema("Exact opaque ETag"), "patch": patch}, "person_id", "etag", "patch")
	input.Defs, patch.Defs = patch.Defs, nil
	input.Definitions, patch.Definitions = patch.Definitions, nil
	return []operationalDefinition{
		newOperationalDefinition("get_person_record", "Read the current structured person profile and exact opaque ETag.", OperationFamilyCurated, closedObject(map[string]*jsonschema.Schema{"person_id": person()}, "person_id"), outputSchemaFor[StructuredPersonRecord](), false),
		newOperationalDefinition("get_person_record_history", "Read current and superseded profile values and source observations.", OperationFamilyCurated, closedObject(map[string]*jsonschema.Schema{"person_id": person()}, "person_id"), outputSchemaFor[generated.PersonProfileHistory](), false),
		newOperationalDefinition("get_person_record_media", "Read stored inline profile media, up to 8 MiB, as base64 with its native content type. URI-only media is unavailable.", OperationFamilyCurated, closedObject(map[string]*jsonschema.Schema{"person_id": person(), "media_id": safeIDSchema("Profile media value ID")}, "person_id", "media_id"), outputSchemaFor[PersonMediaContent](), false),
		newOperationalDefinition("patch_person_record", "Atomically add and supersede structured names, contact points, addresses, dates, categories and media using the exact ETag from get_person_record. Requires approval.", OperationFamilyCurated, input, outputSchemaFor[StructuredPersonRecord](), true),
	}
}
