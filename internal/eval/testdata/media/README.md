This bundle converts the E1 starter corpus to the formats used by msgvault PR #649. See https://github.com/kenn-io/msgvault/pull/649.

`mailbox.json` uses the synthetic mailbox fixture format introduced by PR 649 at `internal/eval/testdata/threaded/mailbox.json`. The message body carries the synthetic transcript text. The two `media-a-v1` messages represent independent occurrences of the same source version. The source version and provenance mapping is:

| source version | message ids | conversation id | provenance |
| --- | --- | --- | --- |
| a-v1 | `<media-a-v1-1@example.test>`, `<media-a-v1-2@example.test>` | `media-a-v1` | provider_transcript |
| a-v2 | `<media-a-v2@example.test>` | `media-a-v2` | provider_transcript |
| b-v1 | `<media-b-v1@example.test>` | `media-b-v1` | generated_transcript |
| c-v1 | `<media-c-v1@example.test>` | `media-c-v1` | provider_transcript |

`topics.tsv` uses the tab-separated topic format accepted by PR 649. The optional third field records the distinction between phrase and semantic questions. q1 uses the starter text `telescope delivery arrives Friday at three` verbatim. `qrels_message.txt` uses the TREC four-field format with message source IDs. `qrels_conversation.txt` uses the same format with conversation source IDs.

The qrels encode the starter judgments at occurrence or conversation level. Message qrels contain ten rows and conversation qrels contain eight rows. Grade `3` means directly relevant; grade `0` means irrelevant. The exact query grades only a-v1. The meaning query grades a-v1, a-v2, and b-v1. c-v1 is a hard negative for both queries.

Msgvault also has `testdata/contextual-eval`, but that corpus uses JSONL scenarios and a separate executable consumer. This bundle uses PR 649's external topics and qrels formats so the media judgments can feed that evaluator after it lands.

This data can be read by the evaluator added in PR #649 after that consumer is available. PR 649's test helper is fixed to `testdata/threaded`, so it does not discover this sibling `testdata/media` directory. Its evaluator command searches an existing archive, so an archive containing the matching synthetic source message or conversation IDs is required for an actual run. This bundle does not add evaluator code, scoring, qrels or topic parsing, search adapters, or Docbank integration. The current evaluator has no field for media occurrence IDs, transcript provenance, source-version fences, replacement, hiding, or presentation-time revocation. Those cases remain in the E1 starter `occurrences.json`, `judgments.json`, and `scenarios.json` files until a media consumer contract can express and execute them.

All values and identities are synthetic. The bundle contains no provider export, personal recording, external API result, embedding output, or measured retrieval score.