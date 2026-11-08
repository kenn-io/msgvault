#!/usr/bin/env python3
"""Run synthetic production-route recording qualification on GitHub CI."""

import argparse
import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path


def await_manifest(path, process, timeout=300):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"fixture process exited {process.returncode} before readiness")
        try:
            return json.loads(path.read_text())
        except (FileNotFoundError, json.JSONDecodeError):
            time.sleep(0.1)
    raise TimeoutError("fixture readiness not observed before deadline")


def stop(process):
    if process is None or process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=15)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=15)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--docbank", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--quality", choices=["deterministic-contract", "real-model"], required=True)
    parser.add_argument("--encoder-python", type=Path)
    args = parser.parse_args()
    if os.environ.get("GITHUB_ACTIONS") != "true":
        parser.error("qualification runs only on a GitHub-hosted runner")
    if args.quality == "real-model" and args.encoder_python is None:
        parser.error("real-model qualification requires --encoder-python")
    root = Path(__file__).resolve().parent.parent
    args.docbank = args.docbank.resolve()
    args.output = args.output.resolve()
    args.output.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    provenance = {
        "msgvault_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip(),
        "docbank_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=args.docbank, text=True).strip(),
        "backend": "postgres" if env.get("MSGVAULT_TEST_DB") else "sqlite",
        "docbank_cgo_enabled": env.get("DOCBANK_EVAL_CGO_ENABLED", "1"),
        "quality": args.quality,
    }
    encoder = None
    with tempfile.TemporaryDirectory(prefix="recording-qualification-") as temporary:
        scratch = Path(temporary)
        try:
            binary = scratch / "docbank-qualification.test"
            docbank_env = {**env, "CGO_ENABLED": provenance["docbank_cgo_enabled"]}
            subprocess.run(
                ["go", "test", "-c", "-tags", "fts5", "-o", str(binary), "./internal/api"],
                cwd=args.docbank, env=docbank_env, check=True,
            )
            if args.quality == "real-model":
                encoder_manifest = scratch / "encoder.json"
                encoder = subprocess.Popen(
                    [str(args.encoder_python), str(root / "scripts/recording-eval-encoder.py"),
                     "--manifest", str(encoder_manifest)], cwd=root, env=env,
                )
                model = await_manifest(encoder_manifest, encoder)
                env["DOCBANK_MEDIA_QUALIFICATION_ENCODER_URL"] = model.pop("url")
                provenance["model"] = model
            (args.output / "provenance.json").write_text(json.dumps(provenance, indent=2) + "\n")
            for mode in ["lexical", "semantic", "hybrid"]:
                manifest_path = scratch / f"{mode}.json"
                child_env = {**env, "CGO_ENABLED": provenance["docbank_cgo_enabled"],
                             "DOCBANK_MEDIA_QUALIFICATION_MANIFEST": str(manifest_path)}
                server = subprocess.Popen(
                    [str(binary), "-test.run", "^TestMediaQualificationServer$", "-test.timeout", "15m"],
                    cwd=args.docbank, env=child_env,
                )
                manifest = None
                try:
                    manifest = await_manifest(manifest_path, server)
                    if manifest["quality"] != args.quality:
                        raise RuntimeError("fixture quality class differs from requested run")
                    run_env = {
                        **env, "MSGVAULT_MEDIA_EVAL_MANIFEST": str(manifest_path),
                        "MSGVAULT_MEDIA_EVAL_MODE": mode, "MSGVAULT_MEDIA_EVAL_OUTPUT": str(args.output / f"{mode}.json"),
                    }
                    subprocess.run(
                        ["go", "test", "-tags", "fts5 sqlite_vec", "-count=1", "-timeout", "12m",
                         "./internal/api", "-run", "^TestMediaRetrievalQualification$", "-v"],
                        cwd=root, env=run_env, check=True,
                    )
                    report = json.loads((args.output / f"{mode}.json").read_text())
                    if report["quality"] != args.quality or not report["metrics"] or not report["scenarios"]:
                        raise RuntimeError("qualification did not produce measured results")
                    if any(scenario["status"] != "passed" for scenario in report["scenarios"]):
                        raise RuntimeError("qualification reported an incomplete lifecycle scenario")
                    request = urllib.request.Request(
                        manifest["url"] + "/qualification/control", data=b'{"action":"stop"}',
                        headers={"Content-Type": "application/json", "X-Api-Key": manifest["api_key"],
                                 "User-Agent": "OpenAI File Downloader, XaiImageApiFetch/1.0"},
                    )
                    with urllib.request.urlopen(request, timeout=15) as response:
                        if response.status != 200:
                            raise RuntimeError("fixture refused shutdown")
                    if server.wait(timeout=30) != 0:
                        raise RuntimeError("Docbank qualification process did not finish cleanly")
                finally:
                    stop(server)
        finally:
            stop(encoder)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, TimeoutError, subprocess.SubprocessError) as error:
        print(f"recording qualification failed: {error}", file=sys.stderr)
        raise SystemExit(1) from error
