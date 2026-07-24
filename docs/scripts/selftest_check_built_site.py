#!/usr/bin/env python3
"""Real-execution self-test for check_built_site's public-site inventory guard.

Drives the actual ``check_public_site_file_inventory`` validator against real
fixture trees on disk (never greps the script) to prove that credential-file
patterns are rejected and that legitimate built-site files are not.
"""
from __future__ import annotations

import contextlib
import importlib.util
import io
import pathlib
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location(
    "check_built_site", HERE / "check_built_site.py"
)
assert _spec is not None and _spec.loader is not None
check_built_site = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(check_built_site)

# One representative file name per forbidden category. Each must be rejected.
FORBIDDEN_FIXTURES = [
    "client_secret_123.json",
    "oauth_client.json",
    "credentials.json",
    "service_account.json",
    "service-account-prod.json",
    "token.json",
    "tokens.json",
    "token-work.json",
    "server.pem",
    "private.key",
    "tls.crt",
    "cert.cer",
    "cert.der",
    "store.p12",
    "store.pfx",
    "AuthKey.p8",
    "keystore.jks",
    "release.keystore",
    "server.ppk",
    "id_rsa",
    "id_rsa.pub",
    "id_dsa",
    "id_ecdsa",
    "id_ed25519",
    "terraform.tfstate",
    "terraform.tfstate.backup",
    "prod.tfvars",
    "CLIENT_SECRET.JSON",  # case-insensitive match
]

# Files a real Zensical build emits; none may be flagged.
ALLOWED_FIXTURES = [
    "index.html",
    "sitemap.xml",
    "style.css",
    "search_index.json",
    "manifest.webmanifest",
    "favicon.png",
    "og-image.svg",
    "app.js",
    "font.woff2",
    "readme.md",
]


def _inventory_rejects(root: pathlib.Path) -> bool:
    # The validator prints to stderr before raising on rejection; swallow that
    # expected noise so the self-test's own output stays readable.
    try:
        with contextlib.redirect_stderr(io.StringIO()):
            check_built_site.check_public_site_file_inventory(root)
    except SystemExit:
        return True
    return False


def main() -> None:
    failures: list[str] = []

    with tempfile.TemporaryDirectory() as tmp:
        clean = pathlib.Path(tmp) / "clean"
        (clean / "assets").mkdir(parents=True)
        for name in ALLOWED_FIXTURES:
            (clean / name).write_text("x", encoding="utf-8")
            (clean / "assets" / name).write_text("x", encoding="utf-8")
        if _inventory_rejects(clean):
            failures.append("clean built-site tree was wrongly rejected")

        for name in FORBIDDEN_FIXTURES:
            case = pathlib.Path(tmp) / f"case_{name.replace('.', '_')}"
            (case / "assets").mkdir(parents=True)
            (case / "index.html").write_text("x", encoding="utf-8")
            # Nest the credential to prove the check inspects every depth.
            (case / "assets" / name).write_text("secret", encoding="utf-8")
            if not _inventory_rejects(case):
                failures.append(f"credential file not rejected: {name}")

        # A dotfile anywhere in the tree must also be rejected.
        dotcase = pathlib.Path(tmp) / "dotcase"
        dotcase.mkdir()
        (dotcase / ".env").write_text("SECRET=1", encoding="utf-8")
        if not _inventory_rejects(dotcase):
            failures.append("dotfile not rejected: .env")

    if failures:
        for message in failures:
            print(f"FAIL: {message}")
        raise SystemExit(1)
    print("check_built_site inventory self-test passed")


if __name__ == "__main__":
    main()
