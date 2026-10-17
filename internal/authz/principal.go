package authz

import "time"

type Kind string

const (
	KindOwner        Kind = "owner"
	KindLoopback     Kind = "loopback"
	KindCaller       Kind = "caller"
	KindRemoteClient Kind = "remote_client"
	KindGrant        Kind = "grant"
	KindKey          Kind = "key"
	KindUser         Kind = "user"
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

// PersonRef binds a selected archive ID to its durable identity, preventing
// reuse of an ID from transferring the former person's authority.
type PersonRef struct {
	ID  int64
	UID string
}

// AddressBookRef binds native account/book identity and account ownership.
type AddressBookRef struct {
	AccountID            int64
	BookID               int64
	CanonicalURL         string
	OwnershipFingerprint string
}

type PersonScope struct {
	Persons []PersonRef
}

type AddressBookScope struct {
	Books []AddressBookRef
}

// Principal is authenticated authority. Has checks capabilities only; callers
// must also authorize the requested sources, people, books and sender identity.
// Credential validation enforces expiry before constructing a principal.
type Principal struct {
	ID          string
	Kind        Kind
	DisplayName string
	Role        Role
	Permissions PermissionSet
	Sources     *SourceScope
	Persons     *PersonScope
	Books       *AddressBookScope
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
