package client

import "go.kenn.io/msgvault/pkg/client/generated"

var (
	_ generated.MuesliRemoteMeetingContactsState = generated.Complete
	_ generated.CardDAVRunResponseErrorCode      = generated.SyncFailed
)
