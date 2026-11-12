package threads

import (
	"errors"
	"net/url"
	"strings"
)

func SourceIdentifier(appID, ownerID string) (string, error) {
	if !tokenValidID(appID) || !tokenValidID(ownerID) {
		return "", errors.New("invalid Threads account identity")
	}
	return "threads:" + appID + ":" + ownerID, nil
}

func ParseSourceIdentifier(identifier string) (appID, ownerID string, err error) {
	return tokenSourceIDs(identifier)
}

func AuthorizationURL(appID, redirectURI, state string) (string, error) {
	if !tokenValidID(appID) {
		return "", errors.New("--app-id must be a nonzero decimal ID of at most 128 digits with no leading zeroes")
	}
	if state == "" {
		return "", errors.New("invalid Threads authorization request")
	}
	if _, err := oauthCallbackURI(redirectURI); err != nil {
		return "", err
	}
	scopes := requiredReadScopes
	query := url.Values{"client_id": {appID}, "redirect_uri": {redirectURI}, "state": {state}, "scope": {strings.Join(scopes, ",")}, "response_type": {"code"}}
	return "https://threads.com/oauth/authorize?" + query.Encode(), nil
}
