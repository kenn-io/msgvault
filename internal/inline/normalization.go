package inline

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const MaxID int64 = 1<<53 - 1

func validID(id int64) bool { return id > 0 && id <= MaxID }

// CanonicalOrigin accepts the production API and MCP origins, and returns the
// common archive namespace. Custom servers require their own identity contract.
func CanonicalOrigin(origin string) (string, error) {
	if origin == ProductionOrigin {
		return ProductionOrigin, nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return "", errors.New("unsupported Inline origin")
	}
	switch strings.ToLower(u.Hostname()) {
	case ProductionOrigin, "mcp.inline.chat":
		return ProductionOrigin, nil
	default:
		return "", errors.New("unsupported Inline origin")
	}
}

func validatePage(page Page, chatID, beforeID int64) error {
	previous := beforeID
	for index, message := range page.Messages {
		if !validID(message.ID) || message.ChatID != chatID || !validID(message.SenderID) {
			return fmt.Errorf("inline message identity does not match selected chat %d", chatID)
		}
		if (index > 0 || beforeID > 0) && message.ID >= previous {
			return fmt.Errorf("inline history does not decrease strictly before %d", previous)
		}
		if message.SentAt.IsZero() || len(message.Raw) == 0 || !json.Valid(message.Raw) {
			return fmt.Errorf("inline message %d lacks timestamp or valid source evidence", message.ID)
		}
		if raw := strings.TrimSpace(string(message.Raw)); len(raw) == 0 || raw[0] != '{' {
			return fmt.Errorf("inline message %d source evidence must be an object", message.ID)
		}
		if message.RawFormat != RawMCPFormat && message.RawFormat != RawCLIFormat {
			return errors.New("unsupported Inline message evidence format")
		}
		if message.ReplyToMessageID < 0 || message.ReplyToMessageID > MaxID {
			return errors.New("invalid Inline reply reference")
		}
		previous = message.ID
	}
	if page.HasMore {
		if len(page.Messages) == 0 || page.NextBeforeID != previous {
			return errors.New("inline history continuation does not match last message")
		}
	} else if page.NextBeforeID != 0 {
		return errors.New("inline history has an unexpected continuation")
	}
	return nil
}
