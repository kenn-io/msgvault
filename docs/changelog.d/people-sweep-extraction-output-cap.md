### Improvements

- People sweeps can give each extraction request a larger output budget with `[people.sweep] extraction_max_output_tokens` (default 4096, at most 32768). Raise it when a reasoning model's answers are cut off and the attempt fails. Keep `max_output_tokens_per_person` at least as large. See [People sweep inference](configuration.md#people-sweep-inference).
