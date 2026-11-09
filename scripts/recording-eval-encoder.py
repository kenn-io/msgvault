#!/usr/bin/env python3
"""Fixture-only real embedding provider for remote recording qualification."""

import argparse
import importlib.metadata
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

MODEL = "sentence-transformers/all-MiniLM-L6-v2"
REVISION = "1110a243fdf4706b3f48f1d95db1a4f5529b4d41"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, required=True)
    args = parser.parse_args()
    if os.environ.get("GITHUB_ACTIONS") != "true":
        parser.error("qualification runs only on a GitHub-hosted runner")

    from sentence_transformers import SentenceTransformer

    model = SentenceTransformer(MODEL, revision=REVISION, device="cpu", trust_remote_code=False)
    metadata = {
        "model": MODEL,
        "revision": REVISION,
        "formatter": "identity; no instruction prefix; normalize_embeddings=true",
        "dimension": 384,
        "metric": "cosine",
        "sentence_transformers": importlib.metadata.version("sentence-transformers"),
        "torch": importlib.metadata.version("torch"),
    }

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def reply(self, status, data):
            raw = json.dumps(data).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self):
            if self.path != "/health":
                self.reply(404, {"error": "not_found"})
                return
            self.reply(200, metadata)

        def do_POST(self):
            if self.path != "/embed":
                self.reply(404, {"error": "not_found"})
                return
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size <= 1024 * 1024:
                    raise ValueError("invalid request size")
                body = json.loads(self.rfile.read(size))
                texts = body["texts"]
                if not isinstance(texts, list) or not 0 < len(texts) <= 128:
                    raise ValueError("expected bounded text array")
                if not all(isinstance(text, str) and len(text.encode()) <= 65536 for text in texts):
                    raise ValueError("invalid text")
                vectors = model.encode(
                    texts, normalize_embeddings=True, convert_to_numpy=True, show_progress_bar=False
                )
                self.reply(200, {"vectors": vectors.tolist()})
            except (ValueError, KeyError, TypeError) as error:
                self.reply(400, {"error": str(error)})

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    args.manifest.parent.mkdir(parents=True, exist_ok=True)
    args.manifest.write_text(json.dumps({"url": f"http://127.0.0.1:{server.server_port}", **metadata}))
    args.manifest.chmod(0o600)
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
