### Bug fixes

- `msgvault person provider add` now works with OpenAI models. When OpenAI rejects the person-fact schema in strict structured-output mode, setup falls back to JSON object output, and msgvault checks the schema locally as before.
