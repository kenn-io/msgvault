package agentdemo

import (
	"context"
	"fmt"
	"time"
)

func voice(ctx context.Context, b *builder) error {
	for _, c := range chatScopes {
		for i := range c.own {
			prefix := c.handle
			switch c.handle {
			case "sam_signal_dm":
				prefix = "sam_signal"
			case "sam_imessage_dm":
				prefix = "sam_imessage"
			}
			h := fmt.Sprintf("%s_own_%d", prefix, i+1)
			body := "The travel notes are ready. I have recorded the details."
			switch c.handle {
			case "weekend_crew":
				body = "weekend plans are ready 🙌 we have a route 😂"
			case "dinner_group":
				body = "sounds good, table is booked"
			case "sam_signal_dm":
				body = "hey, the train details are in my notes"
			}
			at := b.m.AsOf.Add(-time.Duration(c.own-i+3) * 24 * time.Hour)
			if err := b.persistChat(ctx, c, h, c.owner, body, at); err != nil {
				return err
			}
		}
	}
	b.m.Expect["voice"] = map[string]any{
		"min_sample":      5,
		"weekend_crew":    map[string]any{"own_messages": 8, "scope_used": "conversation"},
		"dinner_group":    map[string]any{"own_messages": 2, "scope_used": "source", "skipped": []string{"person"}, "first": []string{"dinner_group_own_1", "dinner_group_own_2"}},
		"sam_signal_dm":   map[string]any{"own_messages": 7, "scope_used": "conversation"},
		"sam_imessage_dm": map[string]any{"own_messages": 2, "scope_used": "person", "first": []string{"sam_imessage_own_1", "sam_imessage_own_2"}},
		"sam_trip_email":  map[string]any{"own_messages": 6, "scope_used": "conversation", "must_include": []string{"sam_trip_sent_from_draft"}, "must_exclude": []string{"sam_trip_unsent_draft"}, "quoted_text_excluded": true},
		"person_sam":      map[string]any{"chat": []string{"sam_signal_dm", "sam_imessage_dm"}, emailKind: []string{"sam_trip_email"}, "never": []string{"weekend_crew", "dinner_group"}},
	}
	return nil
}
