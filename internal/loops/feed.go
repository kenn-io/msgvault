package loops

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

func (d *Deriver) consumeFeed(ctx context.Context, now time.Time, result *RunResult) error {
	state, err := d.State(ctx)
	if err != nil {
		return err
	}
	cursor := store.ChangedMessagesFrom(d.cutoff(now))
	if state.FeedCursorAt != nil {
		cursor = store.ChangedMessagesAfter(*state.FeedCursorAt, state.FeedCursorMessageID)
	} else if state.FeedFrontier != nil {
		cursor = store.ChangedMessagesFrom(*state.FeedFrontier)
	}
	for range d.cfg.FeedPages {
		page, err := d.st.ListChangedMessages(ctx, cursor, d.cfg.BatchMessages)
		if err != nil {
			return err
		}
		frontier := page.CompleteThrough
		ids := []int64{}
		seen := map[int64]bool{}
		for _, message := range page.Messages {
			if message.ConversationID > 0 && !seen[message.ConversationID] {
				seen[message.ConversationID] = true
				ids = append(ids, message.ConversationID)
			}
		}
		if len(page.Messages) > 0 {
			last := page.Messages[len(page.Messages)-1]
			cursor = store.ChangedMessagesAfter(last.ContentChangedAt, last.ID)
			// A full page proves only the exclusive instant before its last row.
			if len(page.Messages) == d.cfg.BatchMessages {
				frontier = last.ContentChangedAt
			}
		} else if _, after := cursor.AfterID(); !after {
			cursor = store.ChangedMessagesFrom(frontier)
		}
		if state.FeedFrontier != nil && frontier.Before(*state.FeedFrontier) {
			frontier = *state.FeedFrontier
		}
		if err := d.st.CommitLoopFeedContext(ctx, ids, cursor, frontier, now); err != nil {
			return err
		}
		result.FeedPages++
		result.ChangedMessages += len(page.Messages)
		state.FeedFrontier = &frontier
		if len(page.Messages) < d.cfg.BatchMessages {
			return nil
		}
	}
	return nil
}
