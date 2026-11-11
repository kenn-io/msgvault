package authz

import "time"

type Kind string

const (
	KindOwner        Kind = "owner"
	KindLoopback     Kind = "loopback"
	KindCaller       Kind = "caller"
	KindRemoteClient Kind = "remote_client"
	KindGrant        Kind = "grant"
	// KindKey is reserved for identity-bound API keys; the daemon API key is
	// currently represented by KindOwner.
	KindKey  Kind = "key"
	KindUser Kind = "user"
)

// SourceScope permits exactly the resolved IDs, including no IDs. A nil
// Principal.Sources means unrestricted access, not an empty selection.
type SourceScope struct {
	IDs []int64
}

// SourceRef matches Type and Identifier across re-adds; ID is diagnostic only.
type SourceRef struct {
	ID         int64
	Type       string
	Identifier string
	SenderKeys []string
}

// Principal is authenticated authority. Has checks capabilities only; callers
// must also authorize the requested sources and sender identity.
// Credential validation enforces expiry before constructing a principal.
type Principal struct {
	ID          string
	Kind        Kind
	DisplayName string
	Role        Role
	Permissions PermissionSet
	Sources     *SourceScope
	UserID      int64
	IssuerID    string
	ExpiresAt   time.Time
}

func (p Principal) Has(permission Permission) bool {
	definition, known := permissionDefinitions[permission]
	if !known {
		return false
	}
	_, explicit := p.Permissions[permission]
	return explicit || roleRank(p.Role) >= roleRank(definition.minimumRole)
}

func (p Principal) Restricted() bool {
	return p.Sources != nil
}
