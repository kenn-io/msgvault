# Context · UTC (set [activity].timezone)
⚠ gmail:owner@example.com stale failed → msgvault sync
⚠ activity reconciling

History
- 13:30 Weekly sync ✓ Alice T1 · notes #21 · 2 actions · possibly related #23 · e12 → get_calendar_event {"ref":"evt:12"}
- 13:30 Notes Alice T1 · 2 actions · #22 → get_meeting_context {"message_ids":[22]}
- Alice T1 · email ↓2 ↑1 · 13:30 "Hello" (2 copies) · burst:1:2:2026-10-22 → list_messages {"conversation_id":2}
- 13:30 message [hidden text] · #32 [filtered] ◀ → get_message {"id":32}
- 13:30 call answered Alice 60s · #41 → get_message {"id":41}

omitted: history 2 · noise 2 · cold 3

retry: meetings

cursor: opaque
