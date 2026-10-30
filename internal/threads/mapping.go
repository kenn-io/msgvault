package threads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/textutil"
)

// snapshotEvidence holds supplied fields separately from raw endpoint JSON.
// Omission preserves evidence; explicit null/false/empty replaces it.
type snapshotEvidence map[string]jsontext.Value

func (e snapshotEvidence) put(key string, value any) {
	data, _ := json.Marshal(value)
	e[key] = data
}
func (e snapshotEvidence) text(key string) string {
	var value string
	_ = json.Unmarshal(e[key], &value)
	return value
}
func (e snapshotEvidence) flag(key string) bool {
	var value bool
	_ = json.Unmarshal(e[key], &value)
	return value
}
func decodeEvidence(data string) (snapshotEvidence, error) {
	e := snapshotEvidence{}
	if data != "" {
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return nil, errors.New("invalid archived Threads metadata")
		}
	}
	return e, nil
}
func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func (e snapshotEvidence) mergeMedia(m Media) {
	strings := map[string]*string{"text": m.Text, "username": m.Username, "permalink": m.Permalink, "media_type": m.MediaType, "media_url": m.MediaURL, "shortcode": m.Shortcode, "hide_status": m.HideStatus}
	for key, value := range strings {
		if value != nil || m.Present[key] {
			e.put(key, value)
		}
	}
	flags := map[string]*bool{"is_quote_post": m.IsQuotePost, "has_replies": m.HasReplies, "is_reply": m.IsReply, "is_reply_owned_by_me": m.IsReplyOwnedByMe}
	for key, value := range flags {
		if value != nil || m.Present[key] {
			e.put(key, value)
		}
	}
	refs := map[string]*MediaReference{"owner": m.Owner, "quoted_post": m.QuotedPost, "reposted_post": m.RepostedPost, "root_post": m.RootPost, "replied_to": m.RepliedTo}
	for key, value := range refs {
		if value != nil || m.Present[key] {
			e.put(key, value)
		}
	}
	if m.Children != nil || m.Present["children"] {
		e.put("children", m.Children)
	}
}

func mediaRoot(m Media, origin ItemOrigin) (string, error) {
	if !validID(m.ID) || m.Timestamp.IsZero() {
		return "", errors.New("Threads item requires a media ID and timestamp")
	}
	for _, ref := range []*MediaReference{m.RootPost, m.RepliedTo, m.Owner, m.QuotedPost, m.RepostedPost} {
		if ref != nil && !validID(ref.ID) {
			return "", errors.New("invalid Threads reference")
		}
	}
	var root string
	switch origin.Kind {
	case "own_posts":
		if m.RepliedTo != nil || (m.IsReply != nil && *m.IsReply) || (m.RootPost != nil && m.RootPost.ID != m.ID) {
			return "", errors.New("own post is not a top-level root")
		}
		root = m.ID
	case "own_replies":
		if m.RootPost == nil {
			return "", errors.New("incomplete Threads own reply: missing root_post")
		}
		root = m.RootPost.ID
		if root == m.ID || (m.IsReply != nil && !*m.IsReply) {
			return "", errors.New("invalid Threads own reply root")
		}
	case "conversation", "sent_provisional":
		root = origin.RootID
		if !validID(root) {
			return "", errors.New("Threads conversation requires a root ID")
		}
	default:
		return "", errors.New("unknown Threads item origin")
	}
	if origin.RootID != "" && origin.RootID != root {
		return "", errors.New("Threads origin root mismatch")
	}
	if m.RootPost != nil && m.RootPost.ID != root {
		return "", errors.New("Threads item root mismatch")
	}
	if m.RepliedTo != nil && (m.RepliedTo.ID == m.ID || m.ID == root) {
		return "", errors.New("invalid Threads parent edge")
	}
	if m.ID == root && m.IsReply != nil && *m.IsReply {
		return "", errors.New("Threads root is a reply")
	}
	return root, nil
}

// validateParent follows only this item's ancestor chain using source-qualified
// point reads. The result is the validated immediate parent's local ID, or zero
// when that parent is not archived. Deferred children are checked at a boundary.
func (w *Writer) validateParent(ctx context.Context, id, root, parent string) (int64, error) {
	seen := map[string]bool{id: true}
	var parentID int64
	for cursor := parent; cursor != ""; {
		if !validID(cursor) {
			return 0, errors.New("invalid archived Threads parent ID")
		}
		if seen[cursor] {
			return 0, errors.New("cyclic Threads parent edge")
		}
		seen[cursor] = true
		record, found, err := w.store.ThreadsArchiveMessageMetadataContext(ctx, w.sourceID, cursor)
		if err != nil {
			return 0, err
		}
		if !found {
			break
		}
		if record.SourceConversationID.String != root {
			return 0, errors.New("cross-conversation Threads parent edge")
		}
		if cursor == parent {
			parentID = record.ID
		}
		e, err := decodeEvidence(record.Metadata.String)
		if err != nil {
			return 0, err
		}
		cursor = e.text("parent_id")
	}
	return parentID, nil
}

func rootTitle(body string) string {
	title := textutil.PrefixRunes(strings.Join(strings.Fields(body), " "), 100)
	if title == "" {
		return "Threads post"
	}
	return title
}

func partialTitle(root string) string { return fmt.Sprintf("Partial Threads context (root %s)", root) }

func rootTitleSnippet(body string) string { return textutil.PrefixRunes(body, 100) }
