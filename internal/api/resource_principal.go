package api

import "errors"

var errIdentityOperationDenied = errors.New("current principal does not authorize every affected identity resource")

func identityOperationPrincipal(auth requestAuthentication) (string, error) {
	switch auth.Mode {
	case AuthModeAPIKey, AuthModeSession, AuthModeLoopback, AuthModeCaller:
		return "owner", nil
	case AuthModeDelegated:
		if auth.Grant != nil && auth.Grant.ID != "" {
			return auth.Grant.ID, nil
		}
	case AuthModeRequired, AuthModeRemoteClient:
		return "", errIdentityOperationDenied
	}
	return "", errIdentityOperationDenied
}
