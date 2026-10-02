package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func importOperationalDefinitions() []operationalDefinition {
	date := func(description string) *jsonschema.Schema {
		return &jsonschema.Schema{Type: mcpSchemaString, Pattern: `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`, Description: description}
	}
	return []operationalDefinition{
		newOperationalDefinition("create_import_job", "Start a historical import for one exact Gmail or IMAP account after approval. Dates are optional; absent or zero limit is unlimited and disclosed explicitly. Acceptance persists a job before scheduling work; it is not completion.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{
			"account":  stringSchema("Exact account identifier or display name, matched case-insensitively; ambiguous selectors are refused"),
			"after":    date("Optional lower date bound; absent means no lower bound"),
			"before":   date("Optional upper date bound; must follow after when both are supplied"),
			"limit":    boundedIntegerSchema("Maximum messages; absent or zero means unlimited", 0, maxJSONSafeInteger),
			"query":    stringSchema("Optional Gmail search query; IMAP refuses nonempty queries"),
			"noresume": booleanSchema("Start without resuming an earlier checkpoint; default false"),
		}, "account"), outputSchemaFor[generated.ImportJobResponse](), true, false),
		newOperationalDefinition("get_import_job", "Read durable import status using the exact job_id from create_import_job. Pending/running are not completion; terminal receipts preserve summary counts and sanitized failure.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{"job_id": stringSchema("Exact opaque job ID from the accepted import")}, "job_id"), outputSchemaFor[generated.ImportJobResponse](), false, false),
	}
}
