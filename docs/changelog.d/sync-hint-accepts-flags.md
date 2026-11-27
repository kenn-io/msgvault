### Bug fixes

- After you add an account, the suggested `msgvault sync-full you@example.com` command no longer starts with `--` before the address, so you can append flags such as `--limit 100`. The `--` still appears when an identifier starts with `-`; put flags before it in that case.
