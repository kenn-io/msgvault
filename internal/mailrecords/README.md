# Transactional mail claims

`Extract(Input)` reads local schema.org JSON-LD from HTML. It supports
`Order`, `ParcelDelivery`, `Invoice`, `FlightReservation`, `LodgingReservation`
and `EventReservation`, including type arrays and `@graph`. It never fetches a
context, follows a URL, calls a provider or writes to the archive.

Each type has a fixed field allowlist. Date, ISO 4217 currency and decimal
values must pass strict validation. Every surviving value is also checked
against the original HTML or plain text. Rejected fields remain null and
increment `dropped`. Text and URLs remain `untrusted_text`; `typed` means
syntactically valid, not authenticated or factually verified.

The caller supplies field origin from separate provenance evidence. An absent
or unsupported origin stays `unknown`. Finding markup alone never establishes
authorship. `markup_only` means an accepted value lacks corroboration in plain
text or HTML text outside script/style elements. It does not model CSS or
browser visibility, and it is an advisory hint rather than an access check.

The parser scans at most 1 MiB of HTML and 1 MiB of optional plain text,
returns at most 20 records, and accepts strings of at most 512 runes. HTML
token count, nesting and JSON traversal are bounded. Oversized or excessively
nested HTML returns an empty record list; malformed JSON-LD blocks are ignored.
Microdata, ICS and vendor-specific prose parsing are separate extensions.
