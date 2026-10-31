package x

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// WriteSummary counts archive projection effects, never billable resources.
// A partial page returns the effects already committed before its error.
type WriteSummary struct{ Added, Updated int64 }

// WritePage persists a page without admission, settlement or cursor changes.
func WritePage(ctx context.Context, st *store.Store, sourceID int64, ownerID string, page Page, fetchedAt time.Time) error {
	_, err := WritePageWithSummary(ctx, st, sourceID, ownerID, page, fetchedAt)
	return err
}

// WritePageWithSummary shares the only page-writing algorithm with WritePage.
// FetchedAt on the original admitted page wins over a local replay timestamp.
func WritePageWithSummary(ctx context.Context, st *store.Store, sourceID int64, ownerID string, page Page, fetchedAt time.Time) (WriteSummary, error) {
	var summary WriteSummary
	if st == nil || sourceID <= 0 || ValidateID(ownerID) != nil {
		return summary, errors.New("invalid X writer source")
	}
	if !page.FetchedAt.IsZero() {
		fetchedAt = page.FetchedAt
	}
	if fetchedAt.IsZero() {
		return summary, errors.New("X page requires its original fetch time")
	}
	users := make(map[string]User, len(page.Users))
	for _, u := range page.Users {
		if ValidateID(u.ID) != nil {
			return summary, errors.New("invalid X expanded author")
		}
		if _, exists := users[u.ID]; exists {
			return summary, errors.New("duplicate X expanded author")
		}
		users[u.ID] = u
	}
	for _, post := range page.Posts {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		retweet := false
		input := store.XPersistPostInput{SourceID: sourceID, OwnerID: ownerID, PostID: post.ID, AuthorID: post.AuthorID, ConversationID: post.ConversationID, EditHistory: post.EditHistory, Text: post.Text, TextIncomplete: post.TextIncomplete, CreatedAt: post.CreatedAt, FetchedAt: fetchedAt, Availability: "unverified"}
		if len(input.EditHistory) == 0 {
			input.EditHistory = []string{post.ID}
		}
		for _, ref := range post.References {
			if ValidateID(ref.ID) != nil {
				return summary, errors.New("invalid X post reference")
			}
			switch ref.Type {
			case "retweeted":
				retweet = true
			case "replied_to":
				if input.ReplyToPostID != "" && input.ReplyToPostID != ref.ID {
					return summary, errors.New("conflicting X reply parents")
				}
				input.ReplyToPostID = ref.ID
			case "quoted":
				if input.QuotedPostID != "" && input.QuotedPostID != ref.ID {
					return summary, errors.New("conflicting X quote references")
				}
				input.QuotedPostID = ref.ID
			default:
				return summary, errors.New("unknown X reference kind")
			}
		}
		if retweet {
			continue
		}
		user, expanded := users[post.AuthorID]
		if !expanded {
			user = User{ID: post.AuthorID}
		}
		author := normalizedXAuthorInput(sourceID, user, fetchedAt)
		input.Author = &author
		if user.Protected {
			input.Availability = "protected"
		} else if user.ProtectedKnown {
			input.Availability = "public"
		}
		if post.Withheld {
			input.Availability = "withheld"
		}
		for _, mention := range post.Mentions {
			if mention.ID != "" {
				input.MentionIDs = append(input.MentionIDs, mention.ID)
			}
		}
		if post.LongText != "" {
			input.Text = post.LongText
		}
		if input.Availability == "protected" || input.Availability == "withheld" {
			input.Text = ""
		} else {
			raw, err := xPostRaw(post, user, expanded)
			if err != nil {
				return summary, fmt.Errorf("prepare X raw post %s: %w", post.ID, err)
			}
			input.Raw = raw
		}
		result, err := st.PersistXPostContext(ctx, input)
		if err != nil {
			return summary, err
		}
		if result.Added {
			summary.Added++
		}
		if result.Updated {
			summary.Updated++
		}
	}
	// Bounded indexed resolution also revisits children from older runs. No
	// checkpoint or spend authority is written by this local pass.
	for cursor := int64(0); ; {
		next, err := st.ResolveXParentsContext(ctx, sourceID, cursor, 100)
		if err != nil {
			return summary, err
		}
		if next == 0 {
			break
		}
		if next <= cursor {
			return summary, errors.New("X parent resolution did not advance")
		}
		cursor = next
	}
	return summary, nil
}

func xPostRaw(post Post, user User, expanded bool) ([]byte, error) {
	type ownAuthor struct {
		ID        string `json:"id"`
		Name      string `json:"name,omitempty"`
		Username  string `json:"username,omitempty"`
		Protected *bool  `json:"protected,omitempty"`
	}
	envelope := struct {
		Post   jsontext.Value `json:"post"`
		Author *ownAuthor     `json:"author,omitempty"`
	}{Post: post.Raw}
	if len(envelope.Post) == 0 {
		raw, err := json.Marshal(map[string]any{"id": post.ID, "author_id": post.AuthorID, "conversation_id": post.ConversationID, "text": post.Text, "created_at": post.CreatedAt, "edit_history_tweet_ids": post.EditHistory})
		if err != nil {
			return nil, err
		}
		envelope.Post = raw
	}
	if expanded {
		envelope.Author = &ownAuthor{ID: user.ID}
		if user.ProtectedKnown || user.Protected {
			value := user.Protected
			envelope.Author.Name = user.Name
			envelope.Author.Username = user.Username
			envelope.Author.Protected = &value
		}
	}
	return json.Marshal(envelope)
}
