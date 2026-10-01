package mcp

import (
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// CardDAVBook declares the additive multi-connection wire fields without
// importing a pending daemon implementation or changing legacy tools.
type CardDAVBook struct {
	ID                 int64  `json:"id"`
	Connection         string `json:"connection,omitempty"`
	Name               string `json:"name"`
	URL                string `json:"url"`
	WriteTarget        bool   `json:"write_target"`
	Subscribed         bool   `json:"subscribed"`
	LookupSource       bool   `json:"lookup_source"`
	NeedsFullReconcile bool   `json:"needs_full_reconcile"`
}
type CardDAVBooks struct {
	Books []CardDAVBook `json:"books"`
}
type CardDAVRun struct {
	ID           int64      `json:"id"`
	AccountID    int64      `json:"account_id,omitempty"`
	Connection   string     `json:"connection,omitempty"`
	Trigger      string     `json:"trigger"`
	Full         bool       `json:"full"`
	State        string     `json:"state"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Books        int64      `json:"books"`
	Created      int64      `json:"created"`
	Updated      int64      `json:"updated"`
	Removed      int64      `json:"removed"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
}
type CardDAVRuns struct {
	Runs         []CardDAVRun `json:"runs"`
	NextBeforeID *int64       `json:"next_before_id,omitempty"`
}
type CardDAVStatus struct {
	Configured           bool                            `json:"configured"`
	Available            bool                            `json:"available"`
	CredentialConfigured bool                            `json:"credential_configured"`
	Enabled              bool                            `json:"enabled"`
	Scheduled            bool                            `json:"scheduled"`
	Schedule             string                          `json:"schedule"`
	NextScheduledAt      *time.Time                      `json:"next_scheduled_at,omitempty"`
	RepairReason         string                          `json:"repair_reason,omitempty"`
	Account              *generated.CardDAVStatusAccount `json:"account,omitempty"`
	Active               *CardDAVRun                     `json:"active,omitempty"`
	Latest               *CardDAVRun                     `json:"latest,omitempty"`
	LatestSuccessful     *CardDAVRun                     `json:"latest_successful,omitempty"`
}
type CardDAVConnection struct {
	Connection string        `json:"connection"`
	AccountID  int64         `json:"account_id,omitempty"`
	Provider   string        `json:"provider,omitempty"`
	OAuthApp   string        `json:"oauth_app,omitempty"`
	Status     CardDAVStatus `json:"status"`
}
type CardDAVConnections struct {
	Connections []CardDAVConnection `json:"connections"`
}
type CardDAVUnpublicationFailure struct {
	Error                     string                                `json:"error"`
	OperationMayHaveCompleted bool                                  `json:"operation_may_have_completed"`
	Publication               *generated.CardDAVPublicationResponse `json:"publication,omitempty"`
}
type CardDAVSync struct {
	Books       int                     `json:"books"`
	Created     int                     `json:"created"`
	Updated     int                     `json:"updated"`
	Removed     int                     `json:"removed"`
	Status      string                  `json:"status,omitempty"`
	Connections []CardDAVConnectionSync `json:"connections,omitempty"`
}
type CardDAVConnectionSync struct {
	Connection   string `json:"connection"`
	AccountID    int64  `json:"account_id,omitempty"`
	RunID        *int64 `json:"run_id,omitempty"`
	Status       string `json:"status"`
	Books        int    `json:"books"`
	Created      int    `json:"created"`
	Updated      int    `json:"updated"`
	Removed      int    `json:"removed"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type CardDAVConnectionCapabilities interface{ SupportsCardDAVConnections() bool }

func cardDAVOperationalDefinitions() []operationalDefinition { return cardDAVDefinitions(false) }

var namedCardDAVDefinitions = sync.OnceValue(func() []operationalDefinition { return cardDAVDefinitions(true) })

func cardDAVDefinitions(named bool) []operationalDefinition {
	scope := func(properties map[string]*jsonschema.Schema) *jsonschema.Schema {
		if properties == nil {
			properties = map[string]*jsonschema.Schema{}
		}
		if named {
			properties["connection"] = &jsonschema.Schema{Type: mcpSchemaString, Pattern: "^[a-z][a-z0-9_-]{0,63}$", Description: "Saved connection name; omit to preserve the daemon's aggregate scope"}
		}
		return closedObject(properties)
	}
	id := func(key string) map[string]*jsonschema.Schema {
		return map[string]*jsonschema.Schema{key: {Type: mcpSchemaInteger, Minimum: new(float64(1))}}
	}
	roleProperties := id("book_id")
	for _, name := range []string{"write_target", "subscribed", "lookup_source"} {
		roleProperties[name] = &jsonschema.Schema{Type: mcpSchemaBoolean}
	}
	resolve := id("conflict_id")
	resolve["choice"] = &jsonschema.Schema{Type: mcpSchemaString, Enum: []any{"keep_local", "keep_remote"}}
	return []operationalDefinition{
		newOperationalDefinition("list_carddav_connections", "List saved CardDAV connections and readiness, without credentials or remote discovery.", OperationFamilyCardDAV, closedObject(nil), outputSchemaFor[CardDAVConnections](), false, false),
		newOperationalDefinition("list_carddav_books", "List the daemon's known CardDAV address books and roles.", OperationFamilyCardDAV, scope(nil), outputSchemaFor[CardDAVBooks](), false, false),
		newOperationalDefinition("list_carddav_runs", "Read durable CardDAV sync history, default25/max100; before_id returns lower run IDs.", OperationFamilyCardDAV, scope(map[string]*jsonschema.Schema{toolArgLimit: {Type: mcpSchemaInteger, Minimum: new(float64(1)), Maximum: new(float64(100))}, "before_id": {Type: mcpSchemaInteger, Minimum: new(float64(1))}}), outputSchemaFor[CardDAVRuns](), false, false),
		newOperationalDefinition("get_carddav_connection_status", "Read aggregate or selected CardDAV readiness and actual active/latest runs.", OperationFamilyCardDAV, scope(nil), outputSchemaFor[CardDAVStatus](), false, false),
		newOperationalDefinition("sync_carddav_connections", "Synchronize enabled connections when scope is omitted; explicit manual scope can synchronize a disabled connection. Retains partial failures and actual started run IDs.", OperationFamilyCardDAV, scope(map[string]*jsonschema.Schema{"full": {Type: mcpSchemaBoolean}}), outputSchemaFor[CardDAVSync](), true, false),
		newOperationalDefinition("update_carddav_book_roles", "Update all three book roles after approval; the daemon enforces one global publication target and its pending-publication switch guard.", OperationFamilyCardDAV, closedObject(roleProperties, "book_id", "write_target", "subscribed", "lookup_source"), outputSchemaFor[CardDAVBook](), true, false),
		newOperationalDefinition("list_carddav_conflicts", "List unresolved CardDAV conflicts with actual allowed resolutions.", OperationFamilyCardDAV, closedObject(nil), outputSchemaFor[generated.CardDAVConflictsResponse](), false, false),
		newOperationalDefinition("get_carddav_conflict", "Read bounded public contact summaries for one actual CardDAV conflict.", OperationFamilyCardDAV, closedObject(id("conflict_id"), "conflict_id"), outputSchemaFor[generated.CardDAVConflictDetailResponse](), false, false),
		newOperationalDefinition("resolve_carddav_conflict", "Resolve one conflict through the daemon's owning connection using an allowed choice after approval.", OperationFamilyCardDAV, closedObject(resolve, "conflict_id", "choice"), outputSchemaFor[generated.CardDAVConflictResolutionResponse](), true, false),
		newOperationalDefinition("unpublish_carddav_person", "Request unpublication after approval, preserving queued delete/remote state. Does not report a pending deletion as completed.", OperationFamilyCardDAV, closedObject(id(toolArgPersonID), toolArgPersonID), outputSchemaFor[generated.CardDAVPublicationResponse](), true, false),
	}
}

func namedCardDAVDefinition(name string) (operationalDefinition, bool) {
	for _, definition := range namedCardDAVDefinitions() {
		if definition.definition.name == name {
			return definition, true
		}
	}
	return operationalDefinition{}, false
}
