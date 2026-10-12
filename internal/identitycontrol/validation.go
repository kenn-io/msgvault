// Package identitycontrol contains validation shared by native resource consumers.
package identitycontrol

import "errors"

const maxSafeIdentityID int64 = 9_007_199_254_740_991

var ErrInvalidRequest = errors.New("invalid identity request")

// ValidID reports whether an archive identifier survives JSON number transport.
func ValidID(id int64) bool {
	return id > 0 && id <= maxSafeIdentityID
}
