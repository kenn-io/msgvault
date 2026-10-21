package msmail

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/oauth2"
)

// Pin native authority before provider IO. A legacy bearer token can archive
// mail without granting Events access; requested OAuth defaults are not evidence.
func observeNativeMailPermission(ctx context.Context, st *store.Store, source *store.Source, c *Client) (*store.NativeSourcePermission, error) {
	resource, err := st.GetMCPResource(ctx, "source", source.ID)
	if err != nil {
		return nil, err
	}
	permission := &store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: SourceType, NativeIdentity: source.Identifier}
	permission.Known, permission.MessageRead = nativeMailGrant(c)
	if permission.MessageRead {
		var profile struct {
			ID                string   `json:"id"`
			Mail              string   `json:"mail"`
			UserPrincipalName string   `json:"userPrincipalName"`
			ProxyAddresses    []string `json:"proxyAddresses"`
		}
		profileErr := c.GetJSON(ctx, "/me?$select=id,mail,userPrincipalName,proxyAddresses", &profile)
		if profileErr != nil {
			recheckErr := recheckNativeMailPermission(ctx, st, permission, c)
			if nativeMailDenied(profileErr) {
				if permission.Known && !permission.MessageRead {
					return permission, errors.Join(profileErr, recheckErr)
				}
				permission.MessageRead = false
				permission.Known = true
				return permission, errors.Join(profileErr, recheckErr, st.ObserveNativeSourcePermission(ctx, *permission))
			}
			return permission, errors.Join(profileErr, recheckErr)
		}
		permission.Known, permission.MessageRead = nativeMailGrant(c)
		if profile.ID == "" || !microsoft.MatchesMailboxIdentity(source.Identifier, profile.Mail, profile.UserPrincipalName, profile.ProxyAddresses) {
			permission.Known, permission.MessageRead = true, false
			return permission, errors.Join(errors.New("native_authority_unavailable"), st.ObserveNativeSourcePermission(ctx, *permission))
		}
	}
	return permission, st.ObserveNativeSourcePermission(ctx, *permission)
}

func nativeMailDenied(err error) bool {
	if errors.Is(err, msgraph.ErrUnauthorized) || errors.Is(err, msgraph.ErrForbidden) {
		return true
	}
	refresh, ok := errors.AsType[*oauth2.RetrieveError](err)
	return ok && refresh.ErrorCode == "invalid_grant"
}

// A live run may lose native grant evidence during token refresh or credential
// replacement. Apply the reduction through its scoped Store before publication.
// New authority is admitted only at the next verified account/profile sync.
func recheckNativeMailPermission(ctx context.Context, st *store.Store, permission *store.NativeSourcePermission, c *Client) error {
	if permission == nil {
		return nil
	}
	known, read := nativeMailGrant(c)
	if read && !permission.MessageRead {
		return nil
	}
	if known == permission.Known && read == permission.MessageRead {
		return nil
	}
	next := *permission
	next.Known, next.MessageRead = known, read
	if err := st.ObserveNativeSourcePermission(ctx, next); err != nil {
		return err
	}
	*permission = next
	return nil
}

func nativeMailGrant(c *Client) (known, read bool) {
	if c.grantedScopes == nil {
		return false, false
	}
	scopes, err := c.grantedScopes()
	if err != nil {
		return false, false
	}
	known = len(scopes) > 0
	for _, scope := range scopes {
		switch scope {
		case "Mail.Read", "Mail.ReadWrite", "https://graph.microsoft.com/Mail.Read", "https://graph.microsoft.com/Mail.ReadWrite":
			read = true
		}
	}
	return known, read
}

// Folder membership is resolved against the inventory admitted before any MIME
// download. Immutable message moves can select another admitted folder, but a
// concurrent replacement cannot lend its generation to this run's response.
func pinNativeMailFolders(ctx context.Context, st *store.Store, permission *store.NativeSourcePermission, folders []Folder) (map[string]store.NativeMessageScopeIdentity, error) {
	pins := make(map[string]store.NativeMessageScopeIdentity, len(folders))
	for _, folder := range folders {
		pin, found, err := st.LookupNativeMessageScope(ctx, permission.SourceID, permission.SourceReference, "msmail_folder", folder.ID)
		if err != nil {
			return nil, err
		}
		if !found || !pin.Present {
			return nil, errors.New("native_folder_unavailable")
		}
		pins[folder.ID] = pin.Identity
	}
	return pins, nil
}
