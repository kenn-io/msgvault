### Upgrade notes

- The CLI now works only with a daemon at its own version or newer. A local background daemon from an older release is replaced automatically, including one from another source build of the same release. A `[remote]` server or `--agent-url` daemon on an older release is refused before any archive request, so upgrade remote servers before or together with the CLIs that use them. Commands no longer fail one feature at a time with "requires daemon API schema" errors.
