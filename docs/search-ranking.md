# Search Ranking Across Backends

`msgvault search` (and the same code path behind the TUI, HTTP API, and
MCP server) uses different ranking functions depending on which database
backend the archive is on. Result *sets* are the same — every message
that matches the query is returned — but the *order* of those results
can differ in adversarial cases. This page explains where and why.

## What each backend does

### SQLite (default)

Ranking is FTS5's `bm25()` function — textbook Okapi BM25 — applied
over the `messages_fts` virtual table. msgvault passes per-column
weights so that hits in subject and sender outrank hits in body and
recipient lists:

```
bm25(messages_fts, 1.0, 10.0, 1.0, 4.0, 1.0, 1.0)
                        ^      ^    ^
                        |      |    +-- from_addr        weight 4
                        |      +------- body             weight 1
                        +-------------- subject          weight 10
```

(The leading `1.0` is the positional slot for the `message_id UNINDEXED`
column; `to_addr` and `cc_addr` follow at weight 1.) Lower BM25 scores
are more relevant; results are ordered ascending.

BM25 also applies document-length normalization: long documents are
penalized so that a query term appearing once in a 50-word email
contributes more than the same term appearing once in a 5,000-word
email.

### PostgreSQL

Ranking is `ts_rank()` over a `tsvector` column on `messages`. Fields
are tagged with PostgreSQL's `setweight` labels (`A` for subject,
`B` for sender, default `D` for body and other recipients). Ranking
uses `ts_rank`'s default normalization flag (`0`), which ignores
document length. The relative weights are PostgreSQL's
defaults — roughly `A=1.0`, `B=0.4`, `D=0.1` — the same 10:4:1 ratio
the SQLite column weights mirror.

`ts_rank` is a cover-density function, not BM25. It cares about how
densely the query terms cluster inside the matched document and which
weighted "buckets" they fall in. It does **not** penalize long
documents.

## Where the two diverge

The SQLite weight tuning makes subject/sender/body ordering line up
with PostgreSQL for typical email shapes. Concretely, given two
messages where one has a query hit only in the subject and the other
only in the body, both backends rank the subject-hit first.

The divergence shows up in adversarial cases that mix field priority
with document length. The clearest example:

- Message **A**: subject matches the query; body is long (a few
  thousand words of quoted thread history, signatures, footers).
- Message **B**: body matches the query (single occurrence); subject
  doesn't match; body is short (one sentence).

PostgreSQL ranks **A** above **B**: subject is the higher-weighted
field and that wins regardless of body length. SQLite's BM25 can
rank **B** above **A**: the length penalty applied to **A**'s long
body shrinks its score enough that **B**'s short-document boost
overtakes the column-weight advantage. Same query, same data, two
different orderings.

This is the scorer functions disagreeing about what relevance means
in a long-document-with-subject-hit vs. short-document-with-body-hit
matchup. It is expected, not a bug.

## Why PostgreSQL's behavior fits email search better

BM25's length normalization is well-motivated for web search. There
the user is *discovering* documents they have never seen, and
"aboutness" — what fraction of the document is on-topic — is a strong
relevance signal. A long page that mentions the query term once is
genuinely less likely to be about the query than a short page where
the same term is half the content.

Email retrieval is a different problem. The user is almost always
*re-finding* a message they already know exists. They are searching
for the meeting confirmation from last March, the receipt for the
flight, the thread where the contract terms got nailed down. They
typically remember a phrase from the **subject line** because the
subject is the user-authored label for what the message is about.

In that setting field structure dominates:

- A subject hit is a strong signal regardless of how long the email
  body is. Subjects are short, deliberate, and chosen as identifiers.
- A body hit is a weaker signal, especially in long threads where
  the query term may be incidental — a quoted reply, a footer, a
  prior thread's signature block.
- Penalizing the subject-hit message because its body is long inverts
  what the user actually wants: the most identifying field gets
  demoted by length characteristics of an unrelated field.

PostgreSQL's `ts_rank` with default normalization respects this. The
weighted bucket the term falls in dominates the score; document length
does not enter the equation. For msgvault's use case — searching a
personal email archive — that is the right tradeoff.

SQLite's BM25 isn't wrong; it's optimized for a different problem
domain (open-corpus document discovery). The column weights msgvault
applies bring SQLite close to PostgreSQL's ordering for ordinary
queries, but they cannot fully suppress length effects in the
adversarial cases above.

## When this matters in practice

For most queries, both backends return the same top results in the
same order. The divergence is observable when:

- The query has hits in several messages with very different body
  lengths, **and**
- At least one of those messages has the hit only in a low-priority
  field (body, recipients), **and**
- You are looking at the top of the result list (the difference
  rarely matters past the first page).

If you are running msgvault on SQLite and noticing a body-hit ranking
above a subject-hit for a query you remember the subject of, this is
why. The match is still there; it's just lower in the list. Use the
`subject:` operator to force a field-restricted query if ordering
matters more than recall:

```
msgvault search 'subject:"quarterly review"'
```

## What strict parity would require

Either a custom SQLite rank function with BM25's length parameter
disabled (`b=0`) loaded via `sqlite3_create_function`, or a
field-priority sort layer applied to BM25 output outside the scorer.
Neither is implemented today; both are out of scope for the current
weight tuning.
