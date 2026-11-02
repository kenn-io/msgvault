// Package tiktokexport imports local TikTok "Download your data" archives.
package tiktokexport

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

type message struct {
	Chat    string    `json:"chat"`
	Sender  string    `json:"sender"`
	Text    string    `json:"text"`
	Link    string    `json:"link,omitempty"`
	Date    time.Time `json:"date"`
	Comment bool      `json:"comment,omitempty"`
	Raw     []byte    `json:"-"`
	Format  string    `json:"-"`
}

func digest(parts ...string) string {
	hash := sha256.New()
	var size [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (m message) key() string {
	return digest(m.conversation(), m.Date.UTC().Format(time.RFC3339Nano), username(m.Sender), m.Text, m.Link)
}
func (m message) conversation() string {
	if m.Comment {
		if m.Link != "" {
			return "comment:" + digest(m.Link)
		}
		return "comments:unlinked"
	}
	return "chat:" + username(m.Chat)
}
func (m message) body() string {
	if m.Link != "" && !strings.Contains(m.Text, m.Link) {
		return m.Text + "\n" + m.Link
	}
	return m.Text
}
func username(s string) string { return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@")) }
func normalized(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

type archive struct {
	Username        string
	Nickname        string
	Messages        []message
	UnknownSections int
	chats           map[string]bool
	comments        bool
	recognized      bool
}

type object map[string]jsontext.Value

func keys(o object) []string {
	out := make([]string, 0, len(o))
	for k := range o {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
func field(o object, names ...string) string {
	for _, name := range names {
		for _, k := range keys(o) {
			if normalized(k) == normalized(name) {
				var s string
				if json.Unmarshal(o[k], &s) == nil {
					return s
				}
			}
		}
	}
	return ""
}
func parseDate(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02 15:04:05 -0700"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("invalid or missing Date; expected YYYY-MM-DD HH:MM:SS or RFC3339")
}
func empty(v jsontext.Value) bool {
	s := strings.TrimSpace(string(v))
	return s == "null" || s == "{}" || s == "[]" || s == `""`
}

func (a *archive) parseJSON(data []byte) error {
	var root object
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if root == nil {
		return errors.New("export JSON must be an object")
	}
	return a.sections(root)
}

// Only named section wrappers are traversed. An unrelated Date/Content object
// in watch history or another activity must never become an archived message.
func (a *archive) sections(o object) error {
	for _, key := range keys(o) {
		value := o[key]
		name := normalized(key)
		next := ""
		switch name {
		case "profile", "profileandsettings", "profileinformation", "profilemap":
			next = "profile"
		case "directmessage", "directmessages", "chathistory":
			next = "dm"
		case "comment", "comments", "commentslist", "commentlist":
			next = "comment"
		case "youractivity", "activity":
			next = "container"
		default:
			a.UnknownSections++
			continue
		}
		a.recognized = true
		if empty(value) {
			continue
		}
		if next == "profile" {
			var sub object
			if err := json.Unmarshal(value, &sub); err != nil {
				return errors.New("profile section must be an object")
			}
			if u := field(sub, "Username", "UserName"); u != "" {
				if a.Username != "" && username(a.Username) != username(u) {
					return errors.New("conflicting profile usernames")
				}
				a.Username = username(u)
				if n := field(sub, "Nickname"); n != "" {
					a.Nickname = n
				}
			} else if err := a.sections(sub); err != nil {
				return err
			}
			continue
		}
		if next == "dm" {
			var sub object
			if err := json.Unmarshal(value, &sub); err != nil {
				return errors.New("direct messages section must be an object")
			}
			for _, chatKey := range keys(sub) {
				if partner, ok := chatPartner(chatKey); ok {
					var records []object
					if err := json.Unmarshal(sub[chatKey], &records); err != nil {
						return errors.New("chat history must contain message arrays")
					}
					if a.chats == nil {
						a.chats = map[string]bool{}
					}
					a.chats[username(partner)] = true
					for _, record := range records {
						if err := a.addRecord(record, partner, false, "tiktok-export-json"); err != nil {
							return err
						}
					}
				} else {
					if err := a.sections(object{chatKey: sub[chatKey]}); err != nil {
						return err
					}
				}
			}
			continue
		}
		if next == "comment" && strings.HasPrefix(strings.TrimSpace(string(value)), "[") {
			var records []object
			if err := json.Unmarshal(value, &records); err != nil {
				return errors.New("comments must contain record objects")
			}
			a.comments = a.comments || len(records) > 0
			for _, record := range records {
				if err := a.addRecord(record, "", true, "tiktok-export-json"); err != nil {
					return err
				}
			}
			continue
		}
		var sub object
		if err := json.Unmarshal(value, &sub); err != nil {
			return fmt.Errorf("%s section must be an object", next)
		}
		if err := a.sections(sub); err != nil {
			return err
		}
	}
	return nil
}

func chatPartner(s string) (string, bool) {
	const prefix = "Chat History with "
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	partner := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s[len(prefix):]), ":"))
	return partner, partner != ""
}
func (a *archive) addRecord(o object, chat string, comment bool, format string) error {
	date, err := parseDate(field(o, "Date"))
	if err != nil {
		return err
	}
	text := field(o, "Content")
	sender := field(o, "From")
	if comment {
		text = field(o, "Comment content", "Comment", "Content")
	}
	if !comment && username(sender) == "" {
		return errors.New("message is missing From")
	}
	link := field(o, "Original post landing page link", "Video Link", "Link", "URL")
	if strings.TrimSpace(text) == "" && strings.TrimSpace(link) == "" {
		return errors.New("message is missing content")
	}
	raw, err := json.Marshal(o, json.Deterministic(true))
	if err != nil {
		return err
	}
	a.Messages = append(a.Messages, message{Chat: chat, Sender: sender, Text: text, Link: link, Date: date, Comment: comment, Raw: raw, Format: format})
	return nil
}
