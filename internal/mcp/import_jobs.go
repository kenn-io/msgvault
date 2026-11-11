package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func importJobDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("create_import_job", "Start an explicitly bounded historical Gmail or IMAP import. Acceptance is not completion; poll the returned job ID. Requires approval.", OperationFamilyImports, closedObject(map[string]*jsonschema.Schema{
			"account":  stringSchema("Exact configured Gmail or IMAP account"),
			"limit":    {Type: "integer", Minimum: new(float64(1)), Description: "Maximum messages to import; required and positive"},
			"after":    stringSchema("Optional lower date bound in YYYY-MM-DD"),
			"before":   stringSchema("Optional upper date bound in YYYY-MM-DD"),
			"query":    stringSchema("Optional Gmail search query"),
			"noresume": booleanSchema("Start without resuming prior progress"),
		}, "account", "limit"), outputSchemaFor[generated.ImportJobResponse](), true),
		newOperationalDefinition("get_import_job", "Read the durable status and summary of one historical import job.", OperationFamilyImports, closedObject(map[string]*jsonschema.Schema{"job_id": stringSchema("Exact job ID from create_import_job")}, "job_id"), outputSchemaFor[generated.ImportJobResponse](), false),
	}
}
