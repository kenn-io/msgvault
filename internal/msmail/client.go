// Package msmail syncs a Microsoft 365 or Outlook.com mailbox through the
// Microsoft Graph mail API. It is the connector for accounts that cannot use
// IMAP, for example a tenant that turned IMAP off.
package msmail

import (
	"context"
	"errors"
	"net/url"
	"time"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/msgraph"
)

// GraphBaseURL is the production Graph endpoint.
const GraphBaseURL = msgraph.GraphBaseURL

// Client adds the mail endpoints to the shared Graph transport.
type Client struct {
	*msgraph.Client
}

// NewClient creates a mail Client. Every request asks for immutable IDs, so a
// message keeps its ID when it moves between folders, and for 1,000-item pages.
func NewClient(baseURL string, token msgraph.TokenFunc, qps float64) *Client {
	c := msgraph.NewClient(baseURL, token, qps)
	c.Headers = map[string]string{"Prefer": `IdType="ImmutableId", odata.maxpagesize=1000`}
	return &Client{c}
}

// Folder is a mail folder. Path joins the display names from the top of the
// mailbox with "/".
type Folder struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ChildFolderCount int    `json:"childFolderCount"`
	Path             string `json:"-"`
}

// DeltaMessage is one item of a folder's message delta. Removed is set when the
// message left the folder: it moved, or it was deleted.
type DeltaMessage struct {
	ID               string    `json:"id"`
	ReceivedDateTime time.Time `json:"receivedDateTime"`
	Removed          *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`

	archiveID int64 // set when the message is already in the vault
}

const folderSelect = "?$top=100&$select=id,displayName,childFolderCount"

// ListFolders returns every folder in the mailbox, parents before children.
func (c *Client) ListFolders(ctx context.Context) ([]Folder, error) {
	var out []Folder
	var walk func(string, string) error
	walk = func(listURL, parent string) error {
		var level []Folder
		if _, err := msgraph.PageThrough(ctx, c.Client, listURL, func(p []Folder) { level = append(level, p...) }); err != nil {
			return err
		}
		for _, f := range level {
			f.Path = f.DisplayName
			if parent != "" {
				f.Path = parent + "/" + f.DisplayName
			}
			out = append(out, f)
			if f.ChildFolderCount > 0 {
				if err := walk("/me/mailFolders/"+url.PathEscape(f.ID)+"/childFolders"+folderSelect, f.Path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return out, walk("/me/mailFolders"+folderSelect, "")
}

// WellKnownFolderID returns the ID of a well-known folder such as "sentitems".
// It returns msgraph.ErrNotFound when the mailbox does not have that folder.
func (c *Client) WellKnownFolderID(ctx context.Context, name string) (string, error) {
	var f Folder
	err := c.GetJSON(ctx, "/me/mailFolders/"+name+"?$select=id", &f)
	return f.ID, err
}

// DeltaStartURL is the first delta request for a folder with no saved cursor.
// It returns every message in the folder and ends with a deltaLink.
func DeltaStartURL(folderID string) string {
	return "/me/mailFolders/" + url.PathEscape(folderID) + "/messages/delta?$select=receivedDateTime"
}

// DeltaPage fetches one page of a delta walk. The page carries a NextLink
// while the walk continues, and a DeltaLink when it is complete.
func (c *Client) DeltaPage(ctx context.Context, pageURL string) (*msgraph.ListResponse[DeltaMessage], error) {
	var page msgraph.ListResponse[DeltaMessage]
	if err := c.GetJSON(ctx, pageURL, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// GetMIME returns the full RFC 5322 source of a message.
func (c *Client) GetMIME(ctx context.Context, id string) ([]byte, error) {
	return c.GetRawWithTimeout(ctx, "/me/messages/"+url.PathEscape(id)+"/$value", 10*time.Minute)
}

// MessageInfo is where a message is now and when it arrived.
type MessageInfo struct {
	ParentFolderID   string    `json:"parentFolderId"`
	ReceivedDateTime time.Time `json:"receivedDateTime"`
}

// LookupMessage returns a message's folder and receipt time. It returns
// msgraph.ErrNotFound when the message no longer exists.
func (c *Client) LookupMessage(ctx context.Context, id string) (MessageInfo, error) {
	var m MessageInfo
	err := c.GetJSON(ctx, "/me/messages/"+url.PathEscape(id)+"?$select=parentFolderId,receivedDateTime", &m)
	return m, err
}

// TrashMessage moves a message to Deleted Items, where the user can restore it.
// It returns *gmail.NotFoundError when the message no longer exists.
func (c *Client) TrashMessage(ctx context.Context, id string) error {
	return c.post(ctx, id, "move", map[string]string{"destinationId": "deleteditems"})
}

// DeleteMessage deletes a message permanently. Graph moves it to the hidden
// Purges folder, and the user cannot restore it. It returns
// *gmail.NotFoundError when the message no longer exists.
func (c *Client) DeleteMessage(ctx context.Context, id string) error {
	return c.post(ctx, id, "permanentDelete", nil)
}

// BatchDeleteMessages is not supported. The deletion executor then deletes
// one message at a time.
func (c *Client) BatchDeleteMessages(context.Context, []string) error {
	return gmail.ErrBatchUnsupported
}

func (c *Client) post(ctx context.Context, id, action string, body any) error {
	path := "/me/messages/" + url.PathEscape(id) + "/" + action
	err := c.Post(ctx, path, body)
	if errors.Is(err, msgraph.ErrNotFound) {
		return &gmail.NotFoundError{Path: path}
	}
	return err
}
