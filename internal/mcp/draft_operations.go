package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"sync"
)

// DraftOutput is the closed union of the existing email, local conversation,
// and native composer receipts. Nullable content preserves withheld text.
type DraftOutput struct {
	Status                           string            `json:"status"`
	DraftID                          string            `json:"draft_id,omitempty"`
	Revision                         int64             `json:"revision,omitempty"`
	SourceID                         int64             `json:"source_id"`
	MessageID                        int64             `json:"message_id,omitempty"`
	Provider                         string            `json:"provider,omitempty"`
	Lifecycle                        string            `json:"lifecycle,omitempty"`
	OperationRef                     string            `json:"operation_ref,omitempty"`
	RFC822MessageID                  string            `json:"rfc822_message_id,omitempty"`
	Mailbox                          string            `json:"mailbox,omitempty"`
	UID                              uint32            `json:"uid,omitempty"`
	UIDValidity                      uint32            `json:"uidvalidity,omitempty"`
	GmailDraftID                     string            `json:"gmail_draft_id,omitempty"`
	GmailMessageID                   string            `json:"gmail_message_id,omitempty"`
	ThreadID                         string            `json:"thread_id,omitempty"`
	Receipt                          *DraftReceipt     `json:"receipt,omitempty"`
	Content                          *string           `json:"content,omitempty"`
	RawMIME                          *string           `json:"raw_mime,omitempty"`
	CandidateContent                 *string           `json:"candidate_content,omitempty"`
	PendingOperation                 string            `json:"pending_operation,omitempty"`
	PendingCode                      string            `json:"pending_code,omitempty"`
	RefusalCode                      string            `json:"refusal_code,omitempty"`
	PendingReceipt                   *DraftReceipt     `json:"pending_receipt,omitempty"`
	PendingReplacementGmailMessageID string            `json:"pending_replacement_gmail_message_id,omitempty"`
	ProviderObservation              *DraftObservation `json:"provider_observation,omitempty"`
	Observation                      *DraftObservation `json:"observation,omitempty"`
	ManualReconciliation             bool              `json:"manual_reconciliation,omitempty"`
	ChatID                           string            `json:"chat_id,omitempty"`
	Location                         string            `json:"location,omitempty"`
	Body                             *string           `json:"body,omitempty"`
	SourceType                       string            `json:"source_type,omitempty"`
	Source                           string            `json:"source,omitempty"`
	ConversationID                   int64             `json:"conversation_id,omitempty"`
	SourceConversationID             string            `json:"source_conversation_id,omitempty"`
	ConversationType                 string            `json:"conversation_type,omitempty"`
	ReplyToSourceMessageID           string            `json:"reply_to_source_message_id,omitempty"`
}

type DraftReceipt struct {
	Mailbox        string `json:"mailbox,omitempty"`
	UID            uint32 `json:"uid,omitempty"`
	UIDValidity    uint32 `json:"uidvalidity,omitempty"`
	GmailDraftID   string `json:"gmail_draft_id,omitempty"`
	GmailMessageID string `json:"gmail_message_id,omitempty"`
	ThreadID       string `json:"thread_id,omitempty"`
}

type DraftObservation struct {
	State          string   `json:"state"`
	Code           string   `json:"code,omitempty"`
	Mailbox        string   `json:"mailbox,omitempty"`
	UID            uint32   `json:"uid,omitempty"`
	UIDValidity    uint32   `json:"uidvalidity,omitempty"`
	GmailDraftID   string   `json:"gmail_draft_id,omitempty"`
	GmailMessageID string   `json:"gmail_message_id,omitempty"`
	ThreadID       string   `json:"thread_id,omitempty"`
	Flags          []string `json:"flags,omitempty"`
	Present        bool     `json:"present"`
	Draft          bool     `json:"draft,omitempty"`
	Deleted        bool     `json:"deleted,omitempty"`
	Complete       bool     `json:"complete,omitempty"`
	UIDPlus        bool     `json:"uidplus,omitempty"`
}

type DraftSendAsOutput struct {
	SourceID int64              `json:"source_id"`
	Account  string             `json:"account"`
	Entries  []DraftSendAsEntry `json:"send_as"`
}

type DraftSendAsEntry struct {
	Email              string `json:"email"`
	DisplayName        string `json:"display_name,omitempty"`
	Primary            bool   `json:"primary"`
	Default            bool   `json:"default"`
	VerificationStatus string `json:"verification_status"`
	ConfirmedIdentity  bool   `json:"confirmed_identity"`
}

// The daemon's actual compose registration determines this optional contract.
type ConversationDraftCapabilities interface{ SupportsConversationDrafts() bool }

func draftInputProperties() map[string]*jsonschema.Schema {
	return map[string]*jsonschema.Schema{
		"source_id": safeIDSchema("Exact destination source ID; mutually exclusive with account"),
		"account":   stringSchema("Exact destination account; mutually exclusive with source_id"),
		"from":      stringSchema("Confirmed sender identity, when required by the provider"),
		"body":      stringSchema("Draft text; an explicit empty value is preserved"),
	}
}

func draftComposeInput(conversation bool) *jsonschema.Schema {
	props := draftInputProperties()
	for _, key := range []string{"to", "cc", "bcc"} {
		props[key] = &jsonschema.Schema{Type: "array", Items: stringSchema("Exact recipient; native Beeper uses one to value naming its chat")}
	}
	props["subject"] = stringSchema("Email subject")
	if conversation {
		props["conversation_id"] = safeIDSchema("Archived conversation for a local draft")
		props["reply_to_message_id"] = safeIDSchema("Optional archived reply target")
	}
	return closedObject(props, "body")
}

var conversationDraftComposeDefinition = sync.OnceValue(func() operationalDefinition {
	return newOperationalDefinition("draft_compose", draftComposeDescription, OperationFamilyDrafts, draftComposeInput(true), outputSchemaFor[DraftOutput](), true, true)
})

const draftComposeDescription = "Create a draft using the daemon's supported destination dispatch. IMAP uses recipients; native Beeper stages text for the user to send, with a read-check/write race. Local conversation drafts stay in msgvault. Gmail fresh compose is unsupported. Never sends a message; requires approval."

func draftOperationalDefinitions() []operationalDefinition {
	reply := draftInputProperties()
	reply["message_id"] = safeIDSchema("Archived parent message ID")
	reply["reply_all"] = booleanSchema("Include parent sender and visible recipients")
	forward := draftInputProperties()
	forward["message_id"] = safeIDSchema("Archived message including its stored attachments")
	for _, key := range []string{"to", "cc", "bcc"} {
		forward[key] = &jsonschema.Schema{Type: "array", Items: stringSchema("Forward recipient")}
	}
	defs := []operationalDefinition{
		newOperationalDefinition("draft_reply", "Create a reply draft, optionally reply-all. Requires approval; never sends a message.", OperationFamilyDrafts, closedObject(reply, "message_id", "body"), outputSchemaFor[DraftOutput](), true, true),
		newOperationalDefinition("draft_compose", draftComposeDescription, OperationFamilyDrafts, draftComposeInput(false), outputSchemaFor[DraftOutput](), true, true),
		newOperationalDefinition("draft_forward", "Create an owner-only forward draft including stored original attachments and a note. Requires approval; never sends a message.", OperationFamilyDrafts, closedObject(forward, "message_id", "body", "to"), outputSchemaFor[DraftOutput](), true, false),
		newOperationalDefinition("get_draft", "Read one managed draft with provider observations and pending state. Withheld content remains null or absent.", OperationFamilyDrafts, closedObject(map[string]*jsonschema.Schema{"draft_id": stringSchema("Exact local draft ID")}, "draft_id"), outputSchemaFor[DraftOutput](), false, true),
		newOperationalDefinition("list_conversation_drafts", "Read local drafts for one archived conversation.", OperationFamilyDrafts, closedObject(map[string]*jsonschema.Schema{"conversation_id": safeIDSchema("Archived conversation ID")}, "conversation_id"), closedObject(map[string]*jsonschema.Schema{"drafts": {Type: "array", Items: outputSchemaFor[DraftOutput]()}}, "drafts"), false, true),
		newOperationalDefinition("list_draft_send_as", "Read Gmail send-as identities for an exact account. Owner-only.", OperationFamilyDrafts, closedObject(map[string]*jsonschema.Schema{"account": stringSchema("Exact Gmail account")}, "account"), outputSchemaFor[DraftSendAsOutput](), false, false),
	}
	for _, name := range []string{"edit_draft", "delete_draft", "recover_draft"} {
		props := map[string]*jsonschema.Schema{"draft_id": stringSchema("Exact local draft ID"), "revision": safeIDSchema("Exact current positive revision")}
		required := []string{"draft_id", "revision"}
		if name == "edit_draft" {
			props["body"] = stringSchema("Replacement text; empty is an explicit replacement")
			required = append(required, "body")
		}
		defs = append(defs, newOperationalDefinition(name, "Operate on the exact managed draft revision. Preserve pending and uncertain provider outcomes; never retries automatically. Requires approval.", OperationFamilyDrafts, closedObject(props, required...), outputSchemaFor[DraftOutput](), true, true))
	}
	return defs
}
