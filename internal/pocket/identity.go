package pocket

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type keyTransport struct {
	base http.RoundTripper
	key  string
}

func (tr keyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+tr.key)
	return tr.base.RoundTrip(clone)
}

func normalizedAccount(account Account) (Account, error) {
	account.Email = strings.ToLower(strings.TrimSpace(account.Email))
	a, err := mail.ParseAddress(account.Email)
	if err != nil || a.Name != "" || a.Address != account.Email || strings.TrimSpace(account.UserID) == "" {
		return Account{}, fmt.Errorf("%w: authenticated account lacks email or userId", ErrContract)
	}
	return account, nil
}

func (c *Client) CurrentAccount(ctx context.Context) (Account, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	httpClient := *c.http
	transport := httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpClient.Transport = keyTransport{base: transport, key: c.key}
	client := mcp.NewClient(&mcp.Implementation{Name: "msgvault", Version: "dev"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: c.mcpURL, HTTPClient: &httpClient, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return Account{}, accountRequestError(ctx)
	}
	defer func() { _ = session.Close() }()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_account_info", Arguments: map[string]any{}})
	if err != nil || result == nil || result.IsError {
		return Account{}, accountRequestError(ctx)
	}
	var raw []byte
	if result.StructuredContent != nil {
		raw, err = json.Marshal(result.StructuredContent)
	} else {
		var text strings.Builder
		for _, item := range result.Content {
			if v, ok := item.(*mcp.TextContent); ok {
				text.WriteString(v.Text)
			}
		}
		raw = []byte(text.String())
	}
	if err != nil || len(raw) > maxEvidenceBytes {
		return Account{}, fmt.Errorf("%w: invalid account result", ErrContract)
	}
	// The tool documents a data wrapper; some MCP deployments return the
	// account object directly. Neither form may assert success=false.
	var reply struct {
		Account

		Success *bool    `json:"success"`
		Data    *Account `json:"data"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Success != nil && !*reply.Success {
		return Account{}, fmt.Errorf("%w: invalid account result", ErrContract)
	}
	account := reply.Account
	if reply.Data != nil {
		account = *reply.Data
	}
	return normalizedAccount(account)
}

func accountRequestError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("pocket account verification failed; check API key and MCP access")
}
