"""Lightweight tests of the actual runner's decision and process boundaries."""
import fcntl
import importlib.util
import json
import pathlib
import sys
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("beeper_morning", pathlib.Path(__file__).with_name("beeper-morning.py"))


class MorningValidation(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.runner = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(cls.runner)

    def test_entire_batch_rejected_before_any_apply(self):
        snapshots = {7: {}, 9: {}}
        for text in [
            '{"decisions":[{"conversation_id":7,"category":"todo","needs_answer":true},{"conversation_id":10,"hold":true}]}',
            '{"decisions":[{"conversation_id":7,"hold":true},{"conversation_id":7,"hold":true}]}',
            '{"decisions":[{"conversation_id":7,"category":"todo","needs_answer":1}]}',
            '{"decisions":[{"conversation_id":7,"hold":true,"command":"send"}]}',
            '{"decisions":[],"decisions":[]}',
        ]:
            with self.subTest(text=text), self.assertRaises(ValueError):
                self.runner.validate_decisions(text, snapshots, 1024)
        got = self.runner.validate_decisions('{"decisions":[{"conversation_id":9,"category":"reference","needs_answer":true,"draft_text":""}]}', snapshots, 1024)
        self.assertEqual("reference", got[0]["category"])
        self.assertTrue(got[0]["needs_answer"])
        with self.assertRaises(ValueError):
            self.runner.validate_decisions('{"decisions":[{"conversation_id":9,"hold":true}]}', {7: {}}, 1024)

    def test_classifier_environment_has_no_owner_authority(self):
        with tempfile.TemporaryDirectory() as tmp:
            env = self.runner.classifier_environment(pathlib.Path(tmp), "ZAI_API_KEY", "synthetic-model-key")
            self.assertEqual("synthetic-model-key", env["ZAI_API_KEY"])
            self.assertEqual(str(pathlib.Path(tmp) / "home"), env["HOME"])
            for forbidden in ["MSGVAULT_API_KEY", "BEEPER_ACCESS_TOKEN", "CODEX_HOME", "NODE_OPTIONS", "SSH_AUTH_SOCK", "HTTP_PROXY"]:
                self.assertNotIn(forbidden, env)
            args = self.runner.classifier_args("/trusted/pi", "zai", "glm-5.3-flash", "max")
            self.assertIn("--no-tools", args)
            self.assertIn("--no-extensions", args)
            self.assertEqual("*", args[args.index("--exclude-tools") + 1])
            self.assertNotIn("--tools", args)

    def test_cli_environment_includes_only_configured_daemon_credentials(self):
        with tempfile.TemporaryDirectory() as tmp:
            pathlib.Path(tmp, "config.toml").write_text(
                '[server]\napi_key_env = "MSGVAULT_API_KEY"\n'
                '[remote]\napi_key_env = "MSGVAULT_REMOTE_KEY"\n',
                encoding="utf-8",
            )
            supplied = {
                "MSGVAULT_API_KEY": "synthetic-server-key",
                "MSGVAULT_REMOTE_KEY": "synthetic-remote-key",
                "BEEPER_ACCESS_TOKEN": "synthetic-provider-token",
                "UNRELATED_SECRET": "must-not-pass-through",
            }
            with mock.patch.dict(self.runner.os.environ, supplied, clear=True):
                env = self.runner.msgvault_cli_environment(pathlib.Path(tmp))
                classifier_env = self.runner.classifier_environment(pathlib.Path(tmp), "ZAI_API_KEY", "synthetic-model-key")

            self.assertEqual("synthetic-server-key", env["MSGVAULT_API_KEY"])
            self.assertEqual("synthetic-remote-key", env["MSGVAULT_REMOTE_KEY"])
            self.assertNotIn("BEEPER_ACCESS_TOKEN", env)
            self.assertNotIn("UNRELATED_SECRET", env)
            self.assertNotIn("MSGVAULT_API_KEY", classifier_env)
            self.assertNotIn("MSGVAULT_REMOTE_KEY", classifier_env)

    def test_scoped_sync_is_explicit_opt_in(self):
        config = pathlib.Path(__file__).parents[1] / "examples/beeper-morning/config.example.json"
        cfg = self.runner.load_config(config)
        controls = ("enabled", "apply", "prepare_drafts", "sync_sources")
        for key in controls:
            self.assertIs(cfg[key], False)
        raw = json.loads(config.read_text())
        for key in controls:
            raw.pop(key, None)
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "defaults.json"
            path.write_text(json.dumps(raw))
            defaults = self.runner.load_config(path)
        for key in controls:
            self.assertIs(defaults[key], False)

    def test_discovery_sync_has_page_granular_limit(self):
        # Pin the external CLI recipe without launching a provider. The built
        # CLI fixture separately exercises this exact production sync path and
        # proves later identity discovery plus durable cursor resumption.
        class ProbeFinished(Exception):
            pass
        with tempfile.TemporaryDirectory() as tmp:
            cfg = dict(self.runner.DEFAULTS, enabled=True, apply=True, sync_sources=True,
                       msgvault_binary="/trusted/msgvault", msgvault_home=tmp, source_ids=[1])
            def observe(argv, env, cwd, text, timeout, cap, operation):
                if operation == "source_identity":
                    return 0, json.dumps({"source_identifier": "synthetic-account"}), ""
                self.assertEqual("source_sync", operation)
                self.assertEqual(["/trusted/msgvault", "--home", tmp, "sync-beeper",
                                  "--account", "synthetic-account", "--no-media", "--limit", "1"], argv)
                raise ProbeFinished
            with mock.patch.object(self.runner, "execute", side_effect=observe), self.assertRaises(ProbeFinished):
                self.runner.run_locked(cfg, pathlib.Path(tmp))

    def test_restored_diagnostics_are_validated_metadata_only(self):
        kept = {"conversation_id": 7, "source_id": 2, "revision": 3, "status": "provider_rejected"}
        report = {"identity": "same", "unresolved": [
            dict(kept, body="private message text", command="send"),
            dict(kept, conversation_id=8, status="archived"),
            dict(kept, conversation_id=9, revision=True),
            dict(kept, conversation_id=10, source_id=99),
            dict(kept, conversation_id=11, status=[]),
        ]}
        self.assertEqual({7: kept}, self.runner.restored_diagnostics(report, "same", [2]))
        self.assertEqual({}, self.runner.restored_diagnostics(report, "other", [2]))

    def test_classifier_incomplete_or_tool_response_fails_closed(self):
        final = {"type": "message_end", "message": {"role": "assistant", "provider": "zai", "model": "fixture", "stopReason": "stop", "content": [{"type": "text", "text": '{"decisions":[]}'}]}}
        settled = {"type": "agent_settled", "aborted": False}
        encode = lambda events: "\n".join(map(json.dumps, events))
        self.assertEqual('{"decisions":[]}', self.runner.classifier_text(encode([final, settled]), "zai", "fixture"))
        for events in [[final], [final, final, settled], [final, dict(settled, aborted=True)],
                       [{"type": "tool_execution_start"}, final, settled],
                       [final, settled, final]]:
            with self.subTest(events=events), self.assertRaises(ValueError):
                self.runner.classifier_text(encode(events), "zai", "fixture")

    def test_real_child_timeout_output_cap_and_failure(self):
        # Real subprocesses exercise the trusted external classifier boundary;
        # no msgvault/provider command is substituted or invoked here.
        env = {"PATH": "/usr/bin:/bin"}
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(ValueError, r"child_timeout; operation=child; elapsed_seconds=[0-9.]+; budget_seconds=0.050; stdout_bytes=0; stderr_bytes=0; output_limit_bytes=1024"):
                self.runner.execute([sys.executable, "-c", "import time;time.sleep(10)"], env, tmp, "", 0.05, 1024)
            with self.assertRaisesRegex(ValueError, r"child_output_limit; operation=child; elapsed_seconds=[0-9.]+; budget_seconds=2.000; stdout_bytes=[0-9]+; stderr_bytes=0; output_limit_bytes=1024") as failed:
                self.runner.execute([sys.executable, "-c", "print('PRIVATE-BODY'*4096)"], env, tmp, "", 2, 1024)
            self.assertNotIn("PRIVATE-BODY", str(failed.exception))
            code, _, _ = self.runner.execute([sys.executable, "-c", "raise SystemExit(7)"], env, tmp, "", 2, 1024)
            self.assertEqual(7, code)

    def test_collection_deadline_preserves_pending_batch_and_classifier_time(self):
        # Exercise the real runner, JSON validation and atomic state files.
        # The existing execute boundary supplies a deterministic external CLI /
        # classifier contract and clock; this is not Store/provider proof.
        for classifier_fails_once in (False, True):
            with self.subTest(classifier_fails_once=classifier_fails_once), tempfile.TemporaryDirectory() as tmp:
                clock = {"now": 0.0}
                rows = {ident: {"conversation_id": ident, "source_id": 1, "chat_id": "synthetic-" + str(ident),
                                "title": "reference", "category": "", "revision": 0, "needs_answer": False,
                                "archive_hold": False, "draft": None} for ident in range(1, 7)}
                classifications = {ident: 0 for ident in rows}
                archives = {ident: 0 for ident in rows}
                model_batches = []
                failed_model = False
                cfg = dict(self.runner.DEFAULTS, enabled=True, apply=True, sync_sources=False,
                           msgvault_binary="/trusted/msgvault", msgvault_home=tmp, pi_binary="/trusted/pi",
                           provider="zai", model="fixture", model_auth_env="ZAI_API_KEY", source_ids=[1],
                           page_size=6, max_chats=6, run_timeout=12, process_timeout=4, context_timeout=4)

                def flag(argv, name):
                    return argv[argv.index(name) + 1]

                def external(argv, env, cwd, text, timeout, cap, operation):
                    nonlocal failed_model
                    self.assertGreater(timeout, 0)
                    self.assertLessEqual(timeout, cfg["process_timeout"])
                    self.assertLessEqual(timeout, cfg["run_timeout"] - clock["now"])
                    cost = {"context_prepare": 2.0, "context_verify": 0.25, "classifier_version": 0.1,
                            "classifier": 1.0, "archive": 0.25}.get(operation, 0.05)
                    if cost > timeout:
                        clock["now"] += timeout
                        raise ValueError("synthetic child timeout")
                    clock["now"] += cost
                    if operation == "catalog":
                        value = {"catalog": [], "has_more": False, "next_cursor": ""}
                    elif operation in ("triage", "answer_groups"):
                        after = int(flag(argv, "--after")) if "--after" in argv else 0
                        limit = int(flag(argv, "--limit"))
                        selected = [dict(row) for ident, row in rows.items() if ident > after]
                        if operation == "answer_groups":
                            selected = [row for row in selected if row["needs_answer"]]
                        value = {"conversations": selected[:limit], "has_more": len(selected) > limit,
                                 "ceiling": 6, "next_after": selected[min(limit, len(selected))-1]["conversation_id"] if selected else after, "source_identifier": "synthetic-account"}
                    elif operation in ("context_prepare", "context_verify"):
                        ident = int(flag(argv, "--conversation"))
                        value = {"complete": True, "changed": rows[ident]["revision"] == 0,
                                 "requests": 5, "stamp": format(ident, "064x"), "activity": "2026-11-01T01:00:00Z",
                                 "head": {"cursor": "event-" + str(ident), "message_id": "event-" + str(ident),
                                          "timestamp": "2026-11-01T01:00:00Z", "message_digest": "d" * 64},
                                 "messages": [{"text": "Synthetic bounded context"}]}
                    elif operation == "classifier_version":
                        return 0, cfg["pi_version"], ""
                    elif operation == "classifier":
                        snapshots = json.loads(text.split("\n", 1)[1])["snapshots"]
                        self.assertEqual(1, len(snapshots), "one conversation's sender text cannot influence another classification")
                        batch = [snap["state"]["conversation_id"] for snap in snapshots]
                        model_batches.append(batch)
                        if classifier_fails_once and not failed_model:
                            failed_model = True
                            return 1, "", ""
                        decisions = {"decisions": [{"conversation_id": ident, "category": "reference",
                                                    "needs_answer": False} for ident in batch]}
                        final = {"type": "message_end", "message": {"role": "assistant", "provider": "zai",
                                 "model": "fixture", "stopReason": "stop",
                                 "content": [{"type": "text", "text": json.dumps(decisions)}]}}
                        return 0, "\n".join(map(json.dumps, [final, {"type": "agent_settled", "aborted": False}])), ""
                    elif operation == "classification":
                        ident = int(flag(argv, "--conversation"))
                        self.assertEqual(rows[ident]["revision"], int(flag(argv, "--revision")))
                        classifications[ident] += 1
                        rows[ident].update(category="reference", revision=rows[ident]["revision"] + 1)
                        value = dict(rows[ident])
                    elif operation == "archive":
                        ident = int(flag(argv, "--conversation"))
                        archives[ident] += 1
                        value = {"status": "archived", "requests": 9}
                    else:
                        self.fail("unexpected runner operation: " + operation)
                    return 0, json.dumps(value), ""

                state_dir = pathlib.Path(tmp)
                first_error = None
                first_report = None
                first_progress = None
                # Six conversations plus one known transient model failure is
                # a finite useful-pass bound, never an until-success loop.
                with mock.patch.object(self.runner.time, "monotonic", side_effect=lambda: clock["now"]), \
                     mock.patch.object(self.runner, "execute", side_effect=external), \
                     mock.patch.dict(self.runner.os.environ, {"ZAI_API_KEY": "synthetic-clock-fixture"}, clear=True):
                    for attempt in range(6 + int(classifier_fails_once)):
                        clock["now"] = 0.0
                        error = None
                        try:
                            self.runner.run_locked(cfg, state_dir)
                        except ValueError as exc:
                            error = str(exc)
                        self.assertLessEqual(clock["now"], cfg["run_timeout"])
                        if attempt == 0:
                            first_error = error
                            report_path = state_dir / "report.json"
                            progress_path = state_dir / "progress.json"
                            first_report = json.loads(report_path.read_text()) if report_path.exists() else None
                            first_progress = json.loads(progress_path.read_text()) if progress_path.exists() else None
                self.assertTrue(model_batches, "collection must leave actual classifier time, not just move a cursor")
                self.assertIsNotNone(first_report, "unfinished admitted work must be durably reported")
                self.assertTrue(first_report["incomplete"])
                self.assertEqual("incomplete", first_report["status"])
                self.assertIsNotNone(first_progress)
                if classifier_fails_once:
                    self.assertIsNone(first_error, "a failed classifier visit must not discard other validated decisions")
                    cursor = first_progress["sources"]["1"]
                    failed_visit = min(model_batches[0])
                    self.assertGreater(cursor["after"], failed_visit,
                                       "later admitted visits may stay pending without rewinding before the failed visit")
                    self.assertEqual(6, cursor["ceiling"], "the unfinished admitted suffix must keep its cohort ceiling")
                    self.assertIn({"conversation_id": failed_visit, "status": "classifier_unavailable"},
                                  first_report["results"])
                    self.assertNotIn({"conversation_id": failed_visit, "status": "classification_deferred"},
                                     first_report["results"])
                else:
                    self.assertIsNone(first_error, "individually bounded collection must yield a useful batch before the run deadline")
                    self.assertGreater(first_progress["sources"]["1"]["after"], 0)
                self.assertEqual(set(rows), {ident for batch in model_batches for ident in batch})
                self.assertEqual({ident: 1 for ident in rows}, classifications, "unchanged restarts cannot reapply a completed decision")
                self.assertEqual({ident: 1 for ident in rows}, archives, "later conversations need useful processing, not cursor-only progress")

    def test_classifier_stage_timeout_applies_completed_decisions_and_defers_rest(self):
        with tempfile.TemporaryDirectory() as tmp:
            clock = {"now": 0.0}
            rows = {ident: {"conversation_id": ident, "source_id": 1, "chat_id": "synthetic-" + str(ident),
                            "title": "reference", "category": "", "revision": 0, "needs_answer": False,
                            "archive_hold": False, "draft": None} for ident in (1, 2)}
            classified = []
            archived = []
            classifier_calls = []
            cfg = dict(self.runner.DEFAULTS, enabled=True, apply=True, sync_sources=False,
                       msgvault_binary="/trusted/msgvault", msgvault_home=tmp, pi_binary="/trusted/pi",
                       provider="zai", model="fixture", model_auth_env="ZAI_API_KEY", source_ids=[1],
                       page_size=2, max_chats=2, max_context_requests=64, run_timeout=6,
                       process_timeout=4, context_timeout=5)

            def flag(argv, name):
                return argv[argv.index(name) + 1]

            def external(argv, env, cwd, text, timeout, cap, operation):
                self.assertGreater(timeout, 0)
                self.assertLessEqual(timeout, cfg["process_timeout"])
                if operation == "classifier":
                    ident = int(json.loads(text.split("\n", 1)[1])["snapshots"][0]["state"]["conversation_id"])
                    classifier_calls.append(ident)
                    cost = 1.0 if ident == 1 else 3.0
                else:
                    cost = {"catalog": 0.05, "triage": 0.05, "context_prepare": 0.5,
                            "classifier_version": 0.05, "context_verify": 0.1,
                            "classification": 0.05, "archive": 0.1, "answer_groups": 0.05}.get(operation, 0.05)
                self.assertLessEqual(cost, cfg["process_timeout"], "each individual classifier call fits the process limit")
                if cost > timeout:
                    clock["now"] += timeout
                    raise ValueError("synthetic child timeout")
                clock["now"] += cost
                if operation == "catalog":
                    value = {"catalog": [], "has_more": False, "next_cursor": ""}
                elif operation in ("triage", "answer_groups"):
                    after = int(flag(argv, "--after")) if "--after" in argv else 0
                    limit = int(flag(argv, "--limit"))
                    selected = [dict(row) for ident, row in rows.items() if ident > after]
                    if operation == "answer_groups":
                        selected = [row for row in selected if row["needs_answer"]]
                    next_after = selected[min(limit, len(selected)) - 1]["conversation_id"] if selected else after
                    value = {"conversations": selected[:limit], "has_more": len(selected) > limit,
                             "ceiling": 2, "next_after": next_after, "source_identifier": "synthetic-account"}
                elif operation in ("context_prepare", "context_verify"):
                    ident = int(flag(argv, "--conversation"))
                    value = {"complete": True, "changed": rows[ident]["revision"] == 0, "requests": 1,
                             "stamp": format(ident, "064x"), "activity": "2026-11-01T01:00:00Z",
                             "head": {"cursor": "event-" + str(ident), "message_id": "event-" + str(ident),
                                      "timestamp": "2026-11-01T01:00:00Z", "message_digest": "d" * 64},
                             "messages": [{"text": "Synthetic bounded context"}]}
                elif operation == "classifier_version":
                    return 0, cfg["pi_version"], ""
                elif operation == "classifier":
                    ident = classifier_calls[-1]
                    decision = {"decisions": [{"conversation_id": ident, "category": "reference",
                                                "needs_answer": False}]}
                    final = {"type": "message_end", "message": {"role": "assistant", "provider": "zai",
                             "model": "fixture", "stopReason": "stop",
                             "content": [{"type": "text", "text": json.dumps(decision)}]}}
                    return 0, "\n".join(map(json.dumps, [final, {"type": "agent_settled", "aborted": False}])), ""
                elif operation == "classification":
                    ident = int(flag(argv, "--conversation"))
                    classified.append(ident)
                    rows[ident].update(category="reference", revision=rows[ident]["revision"] + 1)
                    value = dict(rows[ident])
                elif operation == "archive":
                    ident = int(flag(argv, "--conversation"))
                    archived.append(ident)
                    value = {"status": "archived", "requests": 1}
                else:
                    self.fail("unexpected runner operation: " + operation)
                return 0, json.dumps(value), ""

            state_dir = pathlib.Path(tmp)
            with mock.patch.object(self.runner.time, "monotonic", side_effect=lambda: clock["now"]), \
                 mock.patch.object(self.runner, "execute", side_effect=external), \
                 mock.patch.dict(self.runner.os.environ, {"ZAI_API_KEY": "synthetic-stage-fixture"}, clear=True):
                first = self.runner.run_locked(cfg, state_dir)
                first_progress = json.loads((state_dir / "progress.json").read_text())
                self.assertEqual([1], classified, "the completed first decision is applied before deferring the second")
                self.assertEqual([1], archived)
                second = self.runner.run_locked(cfg, state_dir)

            self.assertTrue(first["incomplete"])
            self.assertEqual(1, first_progress["sources"]["1"]["after"],
                             "the completed first visit stays checkpointed when the next model call exceeds stage time")
            self.assertEqual(2, first_progress["sources"]["1"]["ceiling"])
            self.assertIn({"conversation_id": 2, "status": "classifier_unavailable"}, first["results"])
            self.assertFalse(second["incomplete"], "the deferred conversation must finish on the next run")
            self.assertEqual([1, 2], classified)
            self.assertEqual([1, 2], archived)
            self.assertEqual([1, 2, 2], classifier_calls)

    def test_context_budget_exhaustion_is_a_held_visit(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = dict(self.runner.DEFAULTS, enabled=True, apply=True, sync_sources=False,
                       msgvault_binary="/trusted/msgvault", msgvault_home=tmp, pi_binary="/trusted/pi",
                       provider="zai", model="fixture", model_auth_env="ZAI_API_KEY", source_ids=[1],
                       page_size=1, max_chats=1, max_context_requests=10, run_timeout=30,
                       process_timeout=4, context_timeout=10)
            rows = {ident: {"conversation_id": ident, "source_id": 1, "chat_id": "synthetic-" + str(ident),
                            "category": "", "revision": 0, "needs_answer": False,
                            "archive_hold": False, "draft": None} for ident in (1, 2)}
            classifier_calls = []
            prepare_limits = []
            verify_calls = []
            classified = []
            archived = []
            state_dir = pathlib.Path(tmp)

            def flag(argv, name, default=None):
                return argv[argv.index(name) + 1] if name in argv else default

            def external(argv, env, cwd, text, timeout, cap, operation):
                if operation == "catalog":
                    value = {"catalog": [], "has_more": False, "next_cursor": ""}
                elif operation in ("triage", "answer_groups"):
                    after = int(flag(argv, "--after", 0))
                    limit = int(flag(argv, "--limit"))
                    selected = [dict(row) for ident, row in rows.items() if ident > after]
                    if "--needs-answer" in argv:
                        selected = [row for row in selected if row["needs_answer"]]
                    next_after = selected[min(limit, len(selected)) - 1]["conversation_id"] if selected else after
                    value = {"conversations": selected[:limit], "has_more": len(selected) > limit,
                             "ceiling": 2, "next_after": next_after, "source_identifier": "synthetic-account"}
                elif operation == "context_prepare":
                    ident = int(flag(argv, "--conversation"))
                    requests = int(flag(argv, "--max-requests"))
                    prepare_limits.append((ident, requests))
                    value = {"complete": True, "changed": True, "requests": requests,
                             "stamp": format(ident, "064x"), "activity": "2026-11-01T01:00:00Z",
                             "head": {"cursor": "event-" + str(ident), "message_id": "event-" + str(ident),
                                      "timestamp": "2026-11-01T01:00:00Z", "message_digest": "d" * 64},
                             "messages": [{"text": "Synthetic bounded context"}]}
                elif operation == "classifier_version":
                    return 0, cfg["pi_version"], ""
                elif operation == "classifier":
                    snapshots = json.loads(text.split("\n", 1)[1])["snapshots"]
                    self.assertEqual(1, len(snapshots))
                    ident = snapshots[0]["state"]["conversation_id"]
                    classifier_calls.append(ident)
                    decision = ({"conversation_id": ident, "hold": True} if ident == 2 else
                                {"conversation_id": ident, "category": "reference", "needs_answer": False})
                    decisions = {"decisions": [decision]}
                    final = {"type": "message_end", "message": {"role": "assistant", "provider": "zai",
                             "model": "fixture", "stopReason": "stop",
                             "content": [{"type": "text", "text": json.dumps(decisions)}]}}
                    return 0, "\n".join(map(json.dumps, [final, {"type": "agent_settled", "aborted": False}])), ""
                elif operation == "context_verify":
                    ident = int(flag(argv, "--conversation"))
                    verify_calls.append(ident)
                    if ident == 1:
                        raise ValueError("provider_budget_exhausted")
                    value = {"complete": True, "changed": True, "requests": int(flag(argv, "--max-requests")),
                             "stamp": format(ident, "064x"), "activity": "2026-11-01T01:00:00Z",
                             "head": {"cursor": "event-" + str(ident), "message_id": "event-" + str(ident),
                                      "timestamp": "2026-11-01T01:00:00Z", "message_digest": "d" * 64}}
                elif operation == "classification":
                    ident = int(flag(argv, "--conversation"))
                    classified.append(ident)
                    rows[ident].update(category="reference", revision=rows[ident]["revision"] + 1)
                    value = dict(rows[ident])
                elif operation == "archive":
                    ident = int(flag(argv, "--conversation"))
                    archived.append(ident)
                    value = {"status": "archived", "requests": int(flag(argv, "--max-requests"))}
                else:
                    self.fail("unexpected runner operation: " + operation)
                return 0, json.dumps(value), ""

            with mock.patch.object(self.runner, "execute", side_effect=external), \
                 mock.patch.dict(self.runner.os.environ, {"ZAI_API_KEY": "synthetic-budget-fixture"}, clear=True):
                first = self.runner.run_locked(cfg, state_dir)
                first_progress = json.loads((state_dir / "progress.json").read_text())
                second = self.runner.run_locked(cfg, state_dir)

            self.assertTrue(first["incomplete"])
            self.assertEqual(1, first_progress["sources"]["1"]["after"],
                             "a per-conversation provider budget hold must not rewind the completed visit")
            self.assertEqual([(1, 5), (2, 5)], prepare_limits,
                             "context preparation must reserve verification and archive requests")
            self.assertEqual([1, 2], classifier_calls)
            self.assertEqual([1], verify_calls)
            self.assertEqual([], classified)
            self.assertEqual([], archived)
            self.assertIn({"conversation_id": 2, "status": "held"}, second["results"])
            self.assertFalse(second["incomplete"])

    def test_repeated_classifier_failure_advances_and_revisits_later(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = dict(self.runner.DEFAULTS, enabled=True, apply=True, sync_sources=False,
                       msgvault_binary="/trusted/msgvault", msgvault_home=tmp, pi_binary="/trusted/pi",
                       provider="zai", model="fixture", model_auth_env="ZAI_API_KEY", source_ids=[1],
                       page_size=1, max_chats=1, max_context_requests=19, run_timeout=30,
                       process_timeout=4, context_timeout=10)
            rows = {ident: {"conversation_id": ident, "source_id": 1, "chat_id": "synthetic-" + str(ident),
                            "title": "reference", "category": "", "revision": 0,
                            "needs_answer": False, "archive_hold": False, "draft": None}
                    for ident in (1, 2)}
            classifier_calls = []
            classified = []
            archived = []
            request_accounting = []
            state_dir = pathlib.Path(tmp)

            def flag(argv, name, default=None):
                return argv[argv.index(name) + 1] if name in argv else default

            def context(ident):
                return {"complete": True, "changed": True, "requests": 5,
                        "stamp": format(ident, "064x"), "activity": "2026-11-01T01:00:00Z",
                        "head": {"cursor": "event-" + str(ident), "message_id": "event-" + str(ident),
                                 "timestamp": "2026-11-01T01:00:00Z", "message_digest": "d" * 64},
                        "messages": [{"text": "Synthetic bounded context"}]}

            def external(argv, env, cwd, text, timeout, cap, operation):
                def consume(requests):
                    allowance = int(flag(argv, "--max-requests"))
                    self.assertGreaterEqual(allowance, requests,
                                            operation + " must receive its real request budget")
                    request_accounting.append((operation, allowance, requests))
                    return requests

                if operation == "catalog":
                    value = {"catalog": [], "has_more": False, "next_cursor": ""}
                elif operation in ("triage", "answer_groups"):
                    after = int(flag(argv, "--after", 0))
                    limit = int(flag(argv, "--limit"))
                    selected = [dict(row) for ident, row in rows.items() if ident > after]
                    if "--needs-answer" in argv:
                        selected = [row for row in selected if row["needs_answer"]]
                    next_after = selected[min(limit, len(selected)) - 1]["conversation_id"] if selected else after
                    value = {"conversations": selected[:limit], "has_more": len(selected) > limit,
                             "ceiling": 2, "next_after": next_after}
                elif operation == "context_prepare":
                    value = context(int(flag(argv, "--conversation")))
                    value["requests"] = consume(5)
                elif operation == "classifier_version":
                    return 0, cfg["pi_version"], ""
                elif operation == "classifier":
                    ident = json.loads(text.split("\n", 1)[1])["snapshots"][0]["state"]["conversation_id"]
                    classifier_calls.append(ident)
                    if ident == 1:
                        return 1, "", ""
                    decision = {"decisions": [{"conversation_id": ident, "category": "reference",
                                                "needs_answer": False}]}
                    final = {"type": "message_end", "message": {"role": "assistant", "provider": "zai",
                             "model": "fixture", "stopReason": "stop",
                             "content": [{"type": "text", "text": json.dumps(decision)}]}}
                    return 0, "\n".join(map(json.dumps, [final, {"type": "agent_settled", "aborted": False}])), ""
                elif operation == "context_verify":
                    value = context(int(flag(argv, "--conversation")))
                    value["requests"] = consume(5)
                elif operation == "classification":
                    ident = int(flag(argv, "--conversation"))
                    classified.append(ident)
                    rows[ident].update(category="reference", revision=rows[ident]["revision"] + 1)
                    value = dict(rows[ident])
                elif operation == "archive":
                    archived.append(int(flag(argv, "--conversation")))
                    value = {"status": "archived", "requests": consume(9)}
                else:
                    self.fail("unexpected runner operation: " + operation)
                return 0, json.dumps(value), ""

            with mock.patch.object(self.runner, "execute", side_effect=external), \
                 mock.patch.dict(self.runner.os.environ, {"ZAI_API_KEY": "synthetic-retry-fixture"}, clear=True):
                first = self.runner.run_locked(cfg, state_dir)
                first_progress = json.loads((state_dir / "progress.json").read_text())
                second = self.runner.run_locked(cfg, state_dir)
                second_progress = json.loads((state_dir / "progress.json").read_text())
                third = self.runner.run_locked(cfg, state_dir)
                third_progress = json.loads((state_dir / "progress.json").read_text())

            self.assertTrue(first["incomplete"])
            self.assertIn({"conversation_id": 1, "status": "classifier_unavailable"}, first["results"])
            self.assertNotIn({"conversation_id": 1, "status": "classification_deferred"}, first["results"])
            self.assertEqual(1, first_progress["sources"]["1"]["after"])
            self.assertEqual(2, first_progress["sources"]["1"]["ceiling"])
            self.assertFalse(second["incomplete"])
            self.assertEqual(0, second_progress["sources"]["1"]["after"])
            self.assertEqual(0, second_progress["sources"]["1"]["ceiling"])
            self.assertTrue(third["incomplete"])
            self.assertEqual(1, third_progress["sources"]["1"]["after"])
            self.assertEqual([1, 2, 1], classifier_calls)
            self.assertEqual([2], classified)
            self.assertEqual([2], archived)
            self.assertEqual([("context_prepare", 14, 5), ("context_prepare", 14, 5),
                              ("context_verify", 10, 5), ("archive", 9, 9),
                              ("context_prepare", 14, 5)], request_accounting)

    def test_deferred_source_is_prioritized_fairly_through_classification(self):
        with tempfile.TemporaryDirectory() as tmp:
            clock = {"now": 0.0}
            cfg = dict(self.runner.DEFAULTS, enabled=True, apply=True, prepare_drafts=False,
                       sync_sources=False, msgvault_binary="/trusted/msgvault", msgvault_home=tmp,
                       pi_binary="/trusted/pi", provider="zai", model="fixture",
                       model_auth_env="ZAI_API_KEY", source_ids=[1, 2], page_size=1,
                       max_chats=2, max_context_requests=10, run_timeout=20,
                       process_timeout=12, context_timeout=10)
            rows = {
                1: {"conversation_id": 1, "source_id": 1, "chat_id": "synthetic-1",
                    "category": "", "revision": 0, "needs_answer": False,
                    "archive_hold": False, "draft": None},
                2: {"conversation_id": 2, "source_id": 2, "chat_id": "synthetic-2",
                    "category": "", "revision": 0, "needs_answer": False,
                    "archive_hold": False, "draft": None},
            }
            versions = {1: 1, 2: 1}
            state_dir = pathlib.Path(tmp)
            triage_sources = []
            classifier_inputs = []
            completed = []
            calls_this_run = {"count": 0}

            def flag(argv, name, default=None):
                return argv[argv.index(name) + 1] if name in argv else default

            def context(ident):
                version = versions[ident]
                event = f"event-{ident}-v{version}"
                return {"complete": True, "changed": True, "requests": 5,
                        "stamp": format(ident * 100 + version, "064x"),
                        "activity": f"2026-11-01T01:00:{version:02d}Z",
                        "head": {"cursor": event, "message_id": event,
                                 "timestamp": f"2026-11-01T01:00:{version:02d}Z",
                                 "message_digest": str(version) * 64},
                        "messages": [{"text": f"Synthetic changed context {event}"}]}

            def external(argv, env, cwd, text, timeout, cap, operation):
                if operation == "catalog":
                    return 0, json.dumps({"catalog": [], "has_more": False, "next_cursor": ""}), ""
                if operation == "answer_groups":
                    return 0, json.dumps({"conversations": [], "has_more": False, "next_after": 0}), ""
                if operation == "triage":
                    source = int(flag(argv, "--source-id"))
                    triage_sources.append(source)
                    after = int(flag(argv, "--after", 0))
                    limit = int(flag(argv, "--limit"))
                    selected = [dict(row) for ident, row in rows.items()
                                if ident > after and row["source_id"] == source]
                    next_after = selected[min(limit, len(selected)) - 1]["conversation_id"] if selected else after
                    value = {"conversations": selected[:limit], "has_more": len(selected) > limit,
                             "ceiling": max(ident for ident, row in rows.items() if row["source_id"] == source),
                             "next_after": next_after}
                    return 0, json.dumps(value), ""
                if operation == "context_prepare":
                    value = context(int(flag(argv, "--conversation")))
                    value["requests"] = 5
                    return 0, json.dumps(value), ""
                if operation == "classifier_version":
                    return 0, cfg["pi_version"], ""
                if operation == "classifier":
                    snapshot = json.loads(text.split("\n", 1)[1])["snapshots"][0]
                    ident = snapshot["state"]["conversation_id"]
                    event = snapshot["context"]["head"]["cursor"]
                    classifier_inputs.append((ident, event))
                    calls_this_run["count"] += 1
                    if calls_this_run["count"] == 2:
                        clock["now"] += timeout
                        raise ValueError("child_timeout; operation=classifier")
                    clock["now"] += 10
                    completed.append(ident)
                    decision = {"decisions": [{"conversation_id": ident, "hold": True}]}
                    final = {"type": "message_end", "message": {"role": "assistant", "provider": "zai",
                             "model": "fixture", "stopReason": "stop",
                             "content": [{"type": "text", "text": json.dumps(decision)}]}}
                    return 0, "\n".join(map(json.dumps, [final, {"type": "agent_settled", "aborted": False}])), ""
                self.fail("unexpected runner operation: " + operation)

            with mock.patch.object(self.runner, "execute", side_effect=external), \
                 mock.patch.object(self.runner.time, "monotonic", side_effect=lambda: clock["now"]), \
                 mock.patch.dict(self.runner.os.environ, {"ZAI_API_KEY": "synthetic-fairness-fixture"}, clear=True):
                first = self.runner.run_locked(cfg, state_dir)
                first_progress = json.loads((state_dir / "progress.json").read_text())
                versions[1] = 2
                calls_this_run["count"] = 0
                second = self.runner.run_locked(cfg, state_dir)
                second_progress = json.loads((state_dir / "progress.json").read_text())

            self.assertTrue(first["incomplete"])
            self.assertTrue(second["incomplete"])
            self.assertEqual([1, 2, 2, 1], triage_sources)
            self.assertEqual([1, 2], completed,
                             "one inference per run must not let the lower-ID source starve a deferred source")
            self.assertEqual([(1, "event-1-v1"), (2, "event-2-v1"),
                              (2, "event-2-v1"), (1, "event-1-v2")], classifier_inputs)
            self.assertEqual(1, first_progress["next_source"])
            self.assertEqual(1, first_progress["sources"]["2"]["after"])
            self.assertEqual(2, first_progress["sources"]["2"]["ceiling"])
            self.assertIn({"conversation_id": 2, "status": "held"}, second["results"])
            self.assertIn({"conversation_id": 1, "status": "classifier_unavailable"}, second["results"])
            self.assertEqual(0, second_progress["next_source"])

    def test_run_timeout_keeps_admitted_classifier_work_pending(self):
        with tempfile.TemporaryDirectory() as tmp:
            clock = {"now": 0.0}
            cfg = dict(self.runner.DEFAULTS, enabled=True, apply=True, sync_sources=False,
                       msgvault_binary="/trusted/msgvault", msgvault_home=tmp, pi_binary="/trusted/pi",
                       provider="zai", model="fixture", model_auth_env="ZAI_API_KEY", source_ids=[1],
                       page_size=1, max_chats=1, max_context_requests=10, run_timeout=30,
                       process_timeout=4, context_timeout=10)
            row = {"conversation_id": 1, "source_id": 1, "chat_id": "synthetic-1", "category": "",
                   "revision": 0, "needs_answer": False, "archive_hold": False, "draft": None}
            state_dir = pathlib.Path(tmp)
            real_execute = self.runner.execute

            def advance(seconds):
                clock["now"] += seconds

            def flag(argv, name):
                return argv[argv.index(name) + 1]

            def external(argv, env, cwd, text, timeout, cap, operation):
                if operation == "catalog":
                    value = {"catalog": [], "has_more": False, "next_cursor": ""}
                elif operation == "triage":
                    value = {"conversations": [dict(row)], "has_more": False,
                             "ceiling": 1, "next_after": 1}
                elif operation == "context_prepare":
                    value = {"complete": True, "changed": True, "requests": 1,
                             "stamp": "1" * 64, "activity": "2026-11-01T01:00:00Z",
                             "head": {"cursor": "event-1", "message_id": "event-1",
                                      "timestamp": "2026-11-01T01:00:00Z", "message_digest": "d" * 64},
                             "messages": [{"text": "Synthetic bounded context"}]}
                elif operation == "classifier_version":
                    return 0, cfg["pi_version"], ""
                elif operation == "classifier":
                    self.assertEqual(1, len(json.loads(text.split("\n", 1)[1])["snapshots"]))
                    decision = {"decisions": [{"conversation_id": 1, "category": "reference",
                                                "needs_answer": False}]}
                    final = {"type": "message_end", "message": {"role": "assistant", "provider": "zai",
                             "model": "fixture", "stopReason": "stop",
                             "content": [{"type": "text", "text": json.dumps(decision)}]}}
                    # Leave one deterministic second for verification. Its real
                    # child timeout must expire at the run deadline.
                    clock["now"] = cfg["run_timeout"] - 1
                    return 0, "\n".join(map(json.dumps, [final, {"type": "agent_settled", "aborted": False}])), ""
                elif operation == "context_verify":
                    self.assertEqual("1", flag(argv, "--conversation"))
                    return real_execute([sys.executable, "-c", "import time;time.sleep(10)"],
                                        env, cwd, text, timeout, cap, operation)
                elif operation == "answer_groups":
                    value = {"conversations": [], "has_more": False, "next_after": 0}
                else:
                    self.fail("unexpected runner operation: " + operation)
                return 0, json.dumps(value), ""

            with mock.patch.object(self.runner.time, "monotonic", side_effect=lambda: clock["now"]), \
                 mock.patch.object(self.runner.time, "sleep", side_effect=advance), \
                 mock.patch.object(self.runner, "execute", side_effect=external), \
                 mock.patch.dict(self.runner.os.environ, {"ZAI_API_KEY": "synthetic-timeout-fixture"}, clear=True):
                report = self.runner.run_locked(cfg, state_dir)

            progress = json.loads((state_dir / "progress.json").read_text())
            self.assertTrue(report["incomplete"])
            self.assertIn({"conversation_id": 1, "status": "run_deadline"}, report["results"])
            self.assertIn({"conversation_id": 1, "status": "classification_deferred"}, report["results"])
            self.assertEqual(0, progress["sources"]["1"]["after"])
            self.assertEqual(1, progress["sources"]["1"]["ceiling"])

    def test_omissions_crash_and_failed_scan_preserve_honest_progress(self):
        # Exercise current files and the real runner. Only the external CLI /
        # classifier boundary is substituted; real Store delivery faults remain
        # covered by TestBeeperMorningClassificationCompletionBuiltCLI.
        for mode in ("empty", "partial", "commit-crash", "scan-failure", "reconcile-superseded", "unvisited-source"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as tmp:
                cfg = dict(self.runner.DEFAULTS, enabled=True, apply=True, sync_sources=False,
                           msgvault_binary="/trusted/msgvault", msgvault_home=tmp, pi_binary="/trusted/pi",
                           provider="zai", model="fixture", model_auth_env="ZAI_API_KEY", source_ids=[1],
                           page_size=2, max_chats=2, max_context_requests=64)
                count = 6 if mode in ("empty", "partial") else 1
                rows = {i: {"conversation_id": i, "source_id": 1, "chat_id": "synthetic-" + str(i),
                            "category": "", "revision": 0, "needs_answer": False,
                            "archive_hold": False, "draft": None} for i in range(1, count + 1)}
                if mode == "unvisited-source":
                    cfg.update(source_ids=[1, 2], max_chats=1)
                    rows[2] = dict(rows[1], conversation_id=2, source_id=2, chat_id="synthetic-2")
                batches = []
                catalog_sources = set()
                calls = {"classification": 0, "archive": 0, "reconcile": 0}
                state_dir = pathlib.Path(tmp)
                if mode == "reconcile-superseded":
                    rows[1].update(category="reference", revision=1)
                    identity = self.runner.hashlib.sha256(json.dumps({k: cfg[k] for k in (
                        "msgvault_binary", "msgvault_home", "source_ids", "initial_review",
                        "initial_context_messages")}, sort_keys=True).encode()).hexdigest()
                    self.runner.atomic_json(state_dir / "report.json", {"identity": identity, "unresolved": [
                        {"conversation_id": 1, "source_id": 1, "revision": 1, "status": "archive_unavailable"}]})

                def flag(argv, name):
                    return argv[argv.index(name) + 1]

                def external(argv, env, cwd, text, timeout, cap, operation):
                    if operation == "catalog":
                        catalog_sources.add(int(flag(argv, "--source-id")))
                        value = {"catalog": [], "has_more": False, "next_cursor": ""}
                    elif operation in ("triage", "answer_groups"):
                        if mode == "scan-failure" and operation == "triage":
                            raise ValueError("synthetic page failure")
                        after = int(flag(argv, "--after")) if "--after" in argv else 0
                        limit = int(flag(argv, "--limit"))
                        source = int(flag(argv, "--source-id"))
                        selected = [dict(row) for i, row in rows.items() if i > after and row["source_id"] == source]
                        value = {"conversations": [] if operation == "answer_groups" else selected[:limit],
                                 "has_more": len(selected) > limit, "next_after": 0 if operation == "answer_groups" else (selected[min(limit, len(selected))-1]["conversation_id"] if selected else after), "ceiling": max(i for i, row in rows.items() if row["source_id"] == source)}
                    elif operation in ("context_prepare", "context_verify"):
                        i = int(flag(argv, "--conversation"))
                        value = {"complete": True, "changed": rows[i]["revision"] == 0 or mode == "reconcile-superseded", "requests": 5,
                                 "stamp": format(i, "064x"), "activity": "2026-11-01T01:00:00Z",
                                 "head": {"cursor": "head", "message_id": "head",
                                          "timestamp": "2026-11-01T01:00:00Z", "message_digest": "d" * 64},
                                 "messages": [{"text": "Synthetic context"}]}
                    elif operation == "classifier_version":
                        return 0, cfg["pi_version"], ""
                    elif operation == "classifier":
                        batch = [s["state"]["conversation_id"] for s in json.loads(text.split("\n", 1)[1])["snapshots"]]
                        batches.append(batch)
                        decisions = []
                        if mode in ("partial", "unvisited-source"):
                            decisions = [{"conversation_id": batch[0], "hold": True}]
                        elif mode in ("commit-crash", "reconcile-superseded"):
                            decisions = [{"conversation_id": i, "category": "reference", "needs_answer": False} for i in batch]
                        final = {"type": "message_end", "message": {"role": "assistant", "provider": "zai",
                                 "model": "fixture", "stopReason": "stop", "content": [
                                     {"type": "text", "text": json.dumps({"decisions": decisions})}]}}
                        return 0, "\n".join(map(json.dumps, [final, {"type": "agent_settled", "aborted": False}])), ""
                    elif operation == "classification":
                        calls["classification"] += 1
                        rows[1].update(category="reference", revision=rows[1]["revision"] + 1)
                        if mode == "commit-crash":
                            raise KeyboardInterrupt("synthetic termination after committed write")
                        value = dict(rows[1])
                    elif operation == "reconcile":
                        calls["reconcile"] += 1
                        if int(flag(argv, "--revision")) != rows[1]["revision"]:
                            raise ValueError("synthetic stale revision")
                        value = {"status": "not_archived", "requests": 1}
                    elif operation == "archive":
                        calls["archive"] += 1
                        value = {"status": "archived", "requests": 9}
                    else:
                        self.fail("unexpected runner operation: " + operation)
                    return 0, json.dumps(value), ""

                with mock.patch.object(self.runner, "execute", side_effect=external), \
                     mock.patch.dict(self.runner.os.environ, {"ZAI_API_KEY": "synthetic-fixture"}, clear=True):
                    if mode in ("empty", "partial"):
                        for _ in range(3):
                            self.runner.run_locked(cfg, state_dir)
                        self.assertEqual(set(rows), {i for batch in batches for i in batch},
                                         "omitted decisions cannot indefinitely pin the first bounded page")
                        self.assertEqual(0, calls["classification"])
                        self.assertEqual(0, calls["archive"])
                    elif mode == "commit-crash":
                        with self.assertRaises(KeyboardInterrupt):
                            self.runner.run_locked(cfg, state_dir)
                        report = json.loads((state_dir / "report.json").read_text())
                        warning = {"conversation_id": 1, "source_id": 1, "revision": 1,
                                   "status": "classification_changed_or_unavailable"}
                        self.assertEqual([warning], report["unresolved"], "checkpoint uncertainty before the fallible classification call")
                        restarted = self.runner.run_locked(cfg, state_dir)
                        self.assertEqual([warning], restarted["unresolved"])
                        self.assertEqual(1, calls["classification"])
                        self.assertEqual(0, calls["archive"], "an unchanged restart cannot replay unfinished actions")
                    elif mode == "scan-failure":
                        report = self.runner.run_locked(cfg, state_dir)
                        self.assertTrue(report["incomplete"])
                        self.assertEqual("incomplete", report["status"])
                        self.assertEqual(0, report["scanned"])
                        self.assertEqual("available", report["answer_needed_status"])
                    elif mode == "unvisited-source":
                        report = self.runner.run_locked(cfg, state_dir)
                        self.assertTrue(report["incomplete"], "a wrapped first source cannot hide an unvisited configured source")
                        self.assertEqual("applied", report["status"], "an ordinary bounded visit retains its successful apply status")
                        self.assertEqual(1, report["scanned"])
                        self.assertEqual({1}, catalog_sources)
                        self.runner.run_locked(cfg, state_dir)
                        self.assertEqual({1, 2}, catalog_sources, "the next invocation must start with the deferred source")
                    elif mode == "reconcile-superseded":
                        report = self.runner.run_locked(cfg, state_dir)
                        self.assertEqual(1, calls["classification"])
                        self.assertEqual(1, calls["archive"])
                        self.assertEqual(0, calls["reconcile"], "a newer successful decision supersedes the queued diagnostic")
                        self.assertEqual([], report["unresolved"])

    def test_progress_refuses_unknown_layout_before_cli(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = dict(self.runner.DEFAULTS, enabled=True, apply=True, sync_sources=False,
                       msgvault_binary="/trusted/msgvault", msgvault_home=tmp, source_ids=[1])
            calls = []
            def external(argv, env, cwd, text, timeout, cap, operation):
                calls.append(operation)
                value = {"catalog": [], "next_cursor": "", "has_more": False} if operation == "catalog" else {
                    "conversations": [], "ceiling": 0, "has_more": False, "next_after": 0}
                return 0, json.dumps(value), ""
            state_dir = pathlib.Path(tmp)
            with mock.patch.object(self.runner, "execute", side_effect=external):
                self.runner.run_locked(cfg, state_dir)
                path = state_dir / "progress.json"
                valid = json.loads(path.read_text())
                self.assertEqual({"identity", "sources", "next_source"}, set(valid), "no new persisted version mechanism")
                for variant in ("outer", "cursor", "boolean"):
                    with self.subTest(variant=variant):
                        state = json.loads(json.dumps(valid))
                        if variant == "outer":
                            state["future_policy"] = True
                        elif variant == "cursor":
                            state["sources"]["1"]["future_policy"] = True
                        else:
                            state["sources"]["1"]["after"] = True
                        self.runner.atomic_json(path, state)
                        before = path.read_bytes()
                        calls.clear()
                        with self.assertRaisesRegex(ValueError, "unsupported progress"):
                            self.runner.run_locked(cfg, state_dir)
                        self.assertEqual([], calls)
                        self.assertEqual(before, path.read_bytes())
                self.runner.atomic_json(path, valid)
                changed = dict(cfg, source_ids=[2])
                self.runner.run_locked(changed, state_dir)
                reset = json.loads(path.read_text())
                self.assertNotEqual(valid["identity"], reset["identity"])
                self.assertEqual({"2"}, set(reset["sources"]), "a supported layout can reset on a deliberate config change")

    def test_answer_group_keeps_existing_cli_continuation(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = dict(self.runner.DEFAULTS, enabled=True, apply=True, sync_sources=False,
                       msgvault_binary="/trusted/msgvault", msgvault_home=tmp, source_ids=[1])
            # Owner imports/classification may add rows after this run's empty
            # scan cohort. The final local grouping is a separate bounded read.
            rows = [{"conversation_id": i, "source_id": 1, "category": "todo", "needs_answer": True,
                     "draft": None, "revision": 1, "archive_hold": False} for i in range(1, 201)]
            groups = []
            def external(argv, env, cwd, text, timeout, cap, operation):
                if operation == "catalog":
                    value = {"catalog": [], "next_cursor": "", "has_more": False}
                elif operation == "triage":
                    value = {"conversations": [], "ceiling": 0, "has_more": False, "next_after": 0}
                elif operation == "answer_groups":
                    groups.append(operation)
                    value = {"conversations": rows, "ceiling": 201, "has_more": True, "next_after": 200}
                else:
                    self.fail("unexpected runner operation: " + operation)
                return 0, json.dumps(value), ""
            with mock.patch.object(self.runner, "execute", side_effect=external):
                report = self.runner.run_locked(cfg, pathlib.Path(tmp))
            group = report["answer_needed"][0]
            self.assertTrue(group["has_more"])
            self.assertEqual(200, group.get("next_after"), "a truncated group must expose the CLI's continuation")
            self.assertEqual(200, len(group["conversations"]))
            self.assertEqual(["answer_groups"], groups, "forward the existing cursor without extra reads")

    def test_process_lock_refuses_second_run_before_cli_access(self):
        with tempfile.TemporaryDirectory() as tmp:
            with open(pathlib.Path(tmp) / "run.lock", "a") as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                with self.assertRaisesRegex(ValueError, "already active"):
                    self.runner.run({"enabled": True, "state_dir": tmp})


if __name__ == "__main__":
    unittest.main()
