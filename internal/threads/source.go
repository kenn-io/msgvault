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

func AuthorizationURL(appID, redirectURI, state string, allowSend bool) (string, error) {
	if !tokenValidID(appID) || state == "" {
		return "", errors.New("invalid Threads authorization request")
	}
	if _, err := oauthCallbackURI(redirectURI); err != nil {
		return "", err
	}
	scopes := []string{"threads_basic", "threads_read_replies"}
	if allowSend {
		scopes = append(scopes, "threads_content_publish", "threads_manage_replies")
	}
	query := url.Values{"client_id": {appID}, "redirect_uri": {redirectURI}, "state": {state}, "scope": {strings.Join(scopes, ",")}, "response_type": {"code"}}
	return "https://threads.com/oauth/authorize?" + query.Encode(), nil
}
