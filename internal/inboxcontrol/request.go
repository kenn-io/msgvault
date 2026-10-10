package inboxcontrol

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/emailtags"
)

// Operation names one action. No operation accepts a query mutation.
type Operation string

const (
	OpGetCapabilities Operation = "get-capabilities"
	OpGetState        Operation = "get-state"
	OpListFolders     Operation = "list-folders"
	OpTags            Operation = "tags"
	OpArchive         Operation = "archive"
	OpUnarchive       Operation = "unarchive"
	OpSetRead         Operation = "set-read"
	OpSetUnread       Operation = "set-unread"
	OpMove            Operation = "move"
	OpCreateFolder    Operation = "create-folder"
	OpReceiptGet      Operation = "receipt-get"
	OpReconcile       Operation = "reconcile"
)

// SourceIdentity is a source-only operation's exact account binding. Store
// resolution and grant authorization must still verify all caller fields.
type SourceIdentity struct {
	SourceID         int64  `json:"source_id"`
	SourceType       string `json:"source_type"`
	SourceIdentifier string `json:"source_identifier"`
	AccountID        string `json:"account_id"`
}

// Validate rejects incomplete or unsupported source bindings without echoing input.
func (s SourceIdentity) Validate() error {
	if s.SourceID <= 0 || !validIdentity(s.SourceIdentifier) || !validIdentity(s.AccountID) {
		return fmt.Errorf("%w: source binding requires a positive ID and valid source/account identities", ErrInvalid)
	}
	switch s.SourceType {
	case "gmail", sourceTypeIMAP, "msmail", "beeper":
		return nil
	default:
		return fmt.Errorf("%w: unsupported source_type", ErrInvalid)
	}
}

// Folder names an exact provider location, independently of GTD categories.
// Gmail locations are labels; IMAP locations are actual mailboxes.
type Folder struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	ParentID    string `json:"parent_id,omitempty"`
	UIDValidity uint32 `json:"uidvalidity,omitzero" format:"int64" maximum:"4294967295"`
}

// State carries provider observations, never the local UI messages.is_read.
// Nil booleans mean unknown rather than false. ObservedAt is freshness data,
// not part of a semantic state digest.
type State struct {
	Folders           []Folder       `json:"folders,omitempty"`
	ProvisionedFolder *Folder        `json:"provisioned_folder,omitempty"`
	Target            Target         `json:"target"`
	Source            SourceIdentity `json:"source"`
	Inbox             *bool          `json:"inbox,omitempty"`
	Read              *bool          `json:"read,omitempty"`
	MarkedUnread      *bool          `json:"marked_unread,omitempty"`
	Tags              []string       `json:"tags,omitempty"`
	Flags             []string       `json:"flags,omitempty"`
	Location          string         `json:"location,omitempty"`
	Revision          string         `json:"revision,omitempty"`
	LastMessageID     string         `json:"last_message_id,omitempty"`
	ObservedAt        time.Time      `json:"observed_at"`
}

// Request carries one exact action. Preview (DryRun) reads live state without
// requiring an existing plan. Execution requires expected state, a signed
// preview and an idempotency key. Receipt access resolves authority from its
// stored binding, not from additional caller-supplied targets.
// ResolvedFolder is internal dispatch evidence and is never accepted on the wire.
type Request struct {
	ResolvedFolder   *Folder           `json:"-"`
	NativeTagCatalog bool              `json:"-"`
	Operation        Operation         `json:"operation"`
	Target           *Target           `json:"target,omitempty"`
	Source           *SourceIdentity   `json:"source,omitempty"`
	OriginFolder     *Folder           `json:"origin_folder,omitempty"`
	Destination      *Folder           `json:"destination,omitempty"`
	Tags             *emailtags.Change `json:"tags,omitempty"`
	DryRun           bool              `json:"dry_run,omitempty"`
	Expected         *State            `json:"expected,omitempty"`
	PreviewToken     string            `json:"preview_token,omitempty"`
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
	ReceiptID        string            `json:"receipt_id,omitempty"`
}

// IsMutation reports operations that require a signed preview and a receipt.
func (op Operation) IsMutation() bool {
	switch op {
	case OpTags, OpArchive, OpUnarchive, OpSetRead, OpSetUnread, OpMove, OpCreateFolder:
		return true
	default:
		return false
	}
}

// RequiredPermission is the minimum action permission. Receipt operations
// additionally require the original action/source permission and receipt
// principal ownership, as checked by the service after ledger lookup.
func (op Operation) RequiredPermission() (agentgrant.Permission, error) {
	switch op {
	case OpGetCapabilities, OpGetState, OpListFolders, OpReceiptGet, OpReconcile:
		return agentgrant.PermissionInboxRead, nil
	case OpTags:
		return agentgrant.PermissionInboxTag, nil
	case OpArchive, OpUnarchive:
		return agentgrant.PermissionInboxArchive, nil
	case OpSetRead, OpSetUnread:
		return agentgrant.PermissionInboxReadState, nil
	case OpMove:
		return agentgrant.PermissionInboxMove, nil
	case OpCreateFolder:
		return agentgrant.PermissionInboxFolderCreate, nil
	default:
		return "", fmt.Errorf("%w: unknown operation", ErrInvalid)
	}
}

// Validate checks disjoint intent and identity, without authorizing anything.
func (r Request) Validate() error {
	invalid := func(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }
	if r.ResolvedFolder != nil {
		return invalid("resolved folder identity is internal")
	}
	if r.NativeTagCatalog && (r.Operation != OpGetCapabilities || r.Source == nil || r.Target != nil) {
		return invalid("native tag catalog lookup requires a source capability request")
	}
	if _, err := r.Operation.RequiredPermission(); err != nil {
		return err
	}
	receipt := r.Operation == OpReceiptGet || r.Operation == OpReconcile
	sourceOnly := r.Operation == OpGetCapabilities || r.Operation == OpListFolders || r.Operation == OpCreateFolder
	if receipt {
		if !validIdentity(r.ReceiptID) || r.Target != nil || r.Source != nil {
			return invalid("receipt operations require only a receipt identity")
		}
	} else {
		if r.ReceiptID != "" {
			return invalid("receipt identity applies only to receipt operations")
		}
		if sourceOnly {
			if r.Source == nil || r.Target != nil {
				return invalid("folder discovery and creation require only a source binding")
			}
			if err := r.Source.Validate(); err != nil {
				return err
			}
		} else {
			if r.Target == nil || r.Source != nil {
				return invalid("item operations require only an exact target")
			}
			if err := r.Target.Validate(); err != nil {
				return err
			}
		}
	}
	if r.Operation == OpTags {
		if r.Tags == nil || r.Tags.Mailbox != "" || r.Tags.DryRun {
			return invalid("tags require a delta; mailbox and preview belong to the operation envelope")
		}
		if _, err := emailtags.Normalize(*r.Tags, r.Target.SourceType == sourceTypeIMAP); err != nil {
			return invalid("invalid tag delta")
		}
	} else if r.Tags != nil {
		return invalid("tag delta applies only to tagging")
	}
	if r.Operation == OpMove && r.Target.SourceType == "gmail" {
		if r.OriginFolder == nil || !validIdentity(r.OriginFolder.ID) || r.OriginFolder.UIDValidity != 0 || r.OriginFolder.ParentID != "" {
			return invalid("Gmail move requires an exact origin label")
		}
		if r.Destination != nil && r.OriginFolder.ID == r.Destination.ID {
			return invalid("move origin and destination must differ")
		}
	} else if r.OriginFolder != nil {
		return invalid("origin folder applies only to a Gmail label move")
	}
	needsDestination := r.Operation == OpMove || r.Operation == OpCreateFolder || (r.Operation == OpUnarchive && r.Target.SourceType == sourceTypeIMAP)
	if needsDestination {
		if r.Destination == nil {
			return invalid("operation requires an exact destination")
		}
		if r.Operation == OpCreateFolder {
			if !validIdentity(r.Destination.Name) || r.Destination.ID != "" || r.Destination.UIDValidity != 0 {
				return invalid("folder creation requires a name rather than an existing folder identity")
			}
		} else if !validIdentity(r.Destination.ID) {
			return invalid("move requires an existing destination identity")
		}
		if r.Operation == OpUnarchive && r.Target.SourceType == sourceTypeIMAP &&
			(!strings.EqualFold(r.Destination.ID, "INBOX") || r.Destination.UIDValidity == 0) {
			return invalid("IMAP unarchive requires the current INBOX UIDVALIDITY; use move for other destinations")
		}
		if r.Destination.ParentID != "" && !validIdentity(r.Destination.ParentID) {
			return invalid("invalid folder parent")
		}
	} else if r.Destination != nil {
		return invalid("destination applies only to move, IMAP unarchive or folder creation")
	}
	mutation := r.Operation.IsMutation()
	if !mutation {
		if r.Expected != nil || r.PreviewToken != "" || r.IdempotencyKey != "" {
			return invalid("read and receipt operations cannot carry mutation plans")
		}
		return nil
	}
	if r.DryRun {
		if r.Expected != nil || r.PreviewToken != "" || r.IdempotencyKey != "" {
			return invalid("preview cannot carry execution evidence")
		}
		return nil
	}
	if r.Expected == nil || r.Expected.ObservedAt.IsZero() || r.PreviewToken == "" || len(r.PreviewToken) > 16384 || len(r.IdempotencyKey) < 1 || len(r.IdempotencyKey) > 128 || !utf8.ValidString(r.IdempotencyKey) {
		return invalid("execution requires expected state, preview and a bounded UTF-8 idempotency key")
	}
	if sourceOnly {
		if r.Expected.Source != *r.Source || r.Expected.Target != (Target{}) {
			return invalid("expected state belongs to a different source")
		}
	} else if r.Expected.Target != *r.Target {
		return invalid("expected state belongs to a different target")
	}
	return nil
}
