package imazingidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/textimport"
)

// Identity is native CSV identity evidence before archive participant merging.
type Identity struct{ Kind, Value, DisplayName string }

func Normalize(value, displayName string) (Identity, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return Identity{}, errors.New("identity is empty")
	}
	if phone, err := textimport.NormalizePhone(trimmed); err == nil {
		return Identity{Kind: "phone", Value: phone, DisplayName: strings.TrimSpace(displayName)}, nil
	}
	if address, err := mail.ParseAddress(trimmed); err == nil && strings.Contains(address.Address, "@") && strings.EqualFold(address.Address, trimmed) {
		return Identity{Kind: "email", Value: strings.ToLower(address.Address), DisplayName: strings.TrimSpace(displayName)}, nil
	}
	return Identity{Kind: "opaque", Value: trimmed, DisplayName: strings.TrimSpace(displayName)}, nil
}

func NormalizeChat(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func Synthetic(source, chat, category, displayName string) Identity {
	return Identity{Kind: "opaque", Value: strings.Join([]string{"synthetic", source, chat, category, NormalizeChat(displayName)}, ":"), DisplayName: strings.TrimSpace(displayName)}
}

func Hash(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(hash, "%d:", len(part))
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func ConversationKey(chat string) string { return "imazing_csv:" + Hash(NormalizeChat(chat)) }

func MessageBase(chat string, sent time.Time, direction, senderKey, service string) string {
	return Hash(chat, sent.UTC().Format(time.RFC3339Nano), direction, senderKey, service)
}

func OccurrenceID(base string, number int) string {
	return "imazing_csv:" + Hash(base, strconv.Itoa(number))
}

func EvidenceHashes(values [7]string) [7]string {
	for i, value := range values {
		if value != "" {
			values[i] = Hash("imazing-occurrence-evidence-v1", strconv.Itoa(i), value)
		}
	}
	return values
}

// ArchiveBase reconstructs the native identity from an archived raw row. The
// caller must verify the resulting occurrence against its actual stored key.
func ArchiveBase(source string, sent time.Time, service string, raw map[string]string) (string, error) {
	fields := make(map[string]string, len(raw))
	for key, value := range raw {
		normalized := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(key, "\ufeff")))
		if _, exists := fields[normalized]; exists {
			return "", errors.New("duplicate archived CSV header")
		}
		fields[normalized] = value
	}
	chat := strings.TrimSpace(fields["chat session"])
	if chat == "" {
		return "", errors.New("archived CSV chat is unavailable")
	}
	chatKey := ConversationKey(chat)
	direction := strings.ToLower(strings.TrimSpace(fields["type"]))
	var sender Identity
	var err error
	switch direction {
	case "outgoing":
		sender, err = Normalize(source, "")
	case "incoming":
		if id := strings.TrimSpace(fields["sender id"]); id != "" {
			sender, err = Normalize(id, fields["sender name"])
		} else if name := strings.TrimSpace(fields["sender name"]); name != "" {
			sender = Synthetic(source, chatKey, "sender", name)
		} else {
			return "", errors.New("archived CSV sender is unavailable")
		}
	default:
		return "", errors.New("archived CSV direction is unavailable")
	}
	if err != nil {
		return "", errors.New("archived CSV identity is unavailable")
	}
	return MessageBase(chatKey, sent, direction, sender.Kind+":"+sender.Value, service), nil
}
