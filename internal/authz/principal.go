package authz

type Kind string

const (
	KindOwner        Kind = "owner"
	KindLoopback     Kind = "loopback"
	KindCaller       Kind = "caller"
	KindRemoteClient Kind = "remote_client"
	KindGrant        Kind = "grant"
)

// SourceRef matches Type and Identifier across re-adds; ID is diagnostic only.
type SourceRef struct {
	ID         int64
	Type       string
	Identifier string
	SenderKeys []string
}

// Principal is authenticated authority. Has checks capabilities only; callers
// must also authorize the requested sources and sender identity.
type Principal struct {
	ID          string
	Kind        Kind
	DisplayName string
	Role        Role
	Permissions PermissionSet
}

func (p Principal) Has(permission Permission) bool {
	definition, known := permissionDefinitions[permission]
	if !known {
		return false
	}
	_, explicit := p.Permissions[permission]
	return explicit || roleRank(p.Role) >= roleRank(definition.minimumRole)
}
