# Microsoft Teams Meeting Transcripts — Design (stub)

Date: 2026-06-18
Status: Deferred. Split out of the Teams ingestion spec
(`2026-06-18-teams-ingestion-design.md`) after the load-bearing pass.
Needs its own brainstorm + plan before implementation.

## Why separate

The main Teams ingestion build archives chats, channels, and meeting chats.
Transcripts were originally a final phase there, but the load-bearing pass
(2026-06-18, validated against Microsoft Learn docs) showed the delegated
transcript surface is materially more constrained and lower-certainty, so
it deserves its own design rather than riding along.

## Validated facts (carry into the future design)

- **No direct chat→meeting link, and no meeting id on the chat** (LB-C1,
  confirmed). Resolution path:
  1. Read `chat.onlineMeetingInfo.joinWebUrl` from the meeting chat.
  2. `GET /me/onlineMeetings?$filter=JoinWebUrl eq '{url-encoded joinWebUrl}'`
     (delegated `OnlineMeetings.Read`, no admin consent) → onlineMeeting + id.
  3. `GET /me/onlineMeetings/{id}/transcripts` then
     `.../transcripts/{tid}/content?$format=text/vtt`
     (delegated `OnlineMeetingTranscript.Read.All`, **admin consent**).
- **VTT** is the supported format (`.docx` deprecated 2023).

## Coverage limits (the reason it's deferred)

- **Organizer-only (LB-C3, leaning confirmed):** delegated transcript
  *content* is effectively limited to meetings the signed-in user
  organized. Attendee-token access is undocumented and reportedly 403s.
  The application-permission model is organizer/RSC-centric.
- **Expired meetings:** transcripts are unavailable once a meeting has
  expired — a real gap for a long-horizon archive.
- **Calendar-event association required:** ad-hoc meeting chats with no
  scheduled event won't resolve.

## Open questions for the future brainstorm

- Is organizer-only coverage worth a delegated build, or should this use
  **application permissions** (broader coverage, bigger consent/ops, and
  the metered/payment caveat to re-verify)?
- LB-3 live probe: confirm whether an attendee token ever returns
  transcript content, and the practical expiration window.
- Storage shape: transcript text as a message body on the meeting
  conversation vs. a dedicated child record; link (not bytes) to recording.

## Tracking

kata: see the `teams` + `transcripts` labelled issue.
