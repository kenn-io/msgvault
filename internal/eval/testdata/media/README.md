---
last_edited: 2026-11-08
---
# Synthetic recording retrieval corpus

This E1 corpus contains five message occurrences, four conversations and two judged topics. `mailbox.json` stores transcript text and synthetic occurrence/conversation IDs. The two `media-a-v1` messages are independent live occurrences of the same source version. `topics.tsv` has a query ID, query text and category. The qrels use TREC four-field message or conversation judgments: grade `3` is relevant and grade `0` is irrelevant.

| Source version | Message IDs | Conversation | Transcript origin |
|---|---|---|---|
| a-v1 | `<media-a-v1-1@example.test>`, `<media-a-v1-2@example.test>` | media-a-v1 | supplied |
| a-v2 | `<media-a-v2@example.test>` | media-a-v2 | supplied |
| b-v1 | `<media-b-v1@example.test>` | media-b-v1 | generated |
| c-v1 | `<media-c-v1@example.test>` | media-c-v1 | supplied |

The exact query grades only a-v1. The meaning query grades a-v1, a-v2 and b-v1. c-v1 is a hard negative for both topics. The static a-v2 judgment represents a distinct version; actual in-flight replacement is exercised separately by the qualification suite.

## Run the production path

The Recording retrieval qualification workflow runs `scripts/qualify-recording-retrieval.py` against its exact pinned Docbank checkout. Docbank's opt-in `TestMediaQualificationServer` retains synthetic audio and captions through production processing, publishes real embeddings/indexes, proves readiness through real HTTP search and writes a private fixture manifest. Msgvault's `TestMediaRetrievalQualification` uses those actual receipt IDs and digests to seed separate synthetic conversations and occurrence mappings, then invokes its real HTTP search route. Search results are never supplied by a mock.

The runner reads every topic and qrel. It records occurrence and conversation precision/recall at three, deduplicating before cutoff, plus actual modes, profile coverage, hard negatives and lifecycle outcomes. Supplied and generated profiles are reported separately against the full corpus; combining their rankings would conceal incomplete mixed-profile coverage. It checks shared-byte fan-out, different current captions even without matches, empty and unavailable responses, local replacement/hide/revocation/person changes during real HTTP responses, and source replacement/hide/revocation during query encoding. Mutation barriers and control endpoints exist only in the isolated fixture binary. Explicit index refresh happens outside retrieval between scenarios.

`deterministic-contract` uses controlled embeddings to prove authority and presentation contracts. `real-model` uses the separately pinned public encoder and records its model revision, formatter and library versions. Those are different evidence classes. See the [development guide](../../../../docs/development.md#recording-retrieval-qualification) for the supported-source matrix and artifact interpretation. A queued job, skipped external-fixture test or earlier-head result is not an executed final-head qualification.

This suite qualifies recording retrieval and presentation, not importer acquisition or ASR quality. Existing stored-media and recording-reference tests retain those narrower routing contracts. All corpus values are synthetic; no personal recording, provider export or live archive is used. The report contains no fixture API key.
