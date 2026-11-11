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

// Principal identifies an authenticated credential. Authorization remains with
// the credential-specific admission and resource checks.
type Principal struct {
	ID          string
	Kind        Kind
	DisplayName string
}
