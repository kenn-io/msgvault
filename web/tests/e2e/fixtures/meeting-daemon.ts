import { test as base, expect, type Page } from "@playwright/test";
import { spawn, execFile, type ChildProcess } from "node:child_process";
import { access, mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

const run = promisify(execFile);
const repo = resolve(dirname(fileURLToPath(import.meta.url)), "../../../..");
const binary = join(repo, "msgvault");
const apiKey = "synthetic-meeting-acceptance-key";
const userAgent = "OpenAI File Downloader, XaiImageApiFetch/1.0";

type MeetingRef = { message_id: number; source_id: number };
type SeedManifest = {
  meetings: Record<"granola" | "notion" | "circleback", MeetingRef>;
  participant_id: number;
};
export type MeetingDaemon = SeedManifest & {
  origin: string;
  generic: MeetingRef;
  personID: number;
  post: (path: string, data: unknown) => Promise<Response>;
};

// Retain bounded synthetic logs even on setup failure. Only our own child is
// stopped, and all archive/config/runtime state is removed after the test.
async function stop(child: ChildProcess): Promise<void> {
  if (!child.pid || child.exitCode !== null || child.signalCode !== null)
    return;
  const exited = new Promise<void>((resolve) =>
    child.once("exit", () => resolve()),
  );
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), 5_000);
  try {
    await exited;
  } finally {
    clearTimeout(timer);
  }
}

export const test = base.extend<{ daemon: MeetingDaemon }>({
  userAgent,
  daemon: [
    async ({}, use, testInfo) => {
      const scratch = await mkdtemp(join(tmpdir(), "msgvault-meeting-e2e-"));
      const archive = join(scratch, "archive");
      let child: ChildProcess | undefined;
      let logs = "";
      const capture = (data: Buffer) => {
        logs = (logs + data.toString()).slice(-64 * 1024);
      };
      try {
        await mkdir(archive);
        await mkdir(join(scratch, "os-home"));
        const env: NodeJS.ProcessEnv = {
          PATH: process.env.PATH,
          HOME: join(scratch, "os-home"),
          MSGVAULT_HOME: archive,
          XDG_CONFIG_HOME: join(scratch, "xdg-config"),
          XDG_CACHE_HOME: join(scratch, "xdg-cache"),
          TMPDIR: scratch,
          TZ: "UTC",
        };
        // Reuse immutable compiler caches without giving the daemon any ambient
        // provider/remote/archive configuration or credentials.
        const cacheEnv = Object.fromEntries(
          ["PATH", "HOME", "GOPATH", "GOCACHE", "GOMODCACHE"].flatMap((key) =>
            process.env[key] ? [[key, process.env[key]]] : [],
          ),
        );
        const caches = JSON.parse(
          (
            await run("go", ["env", "-json", "GOCACHE", "GOMODCACHE"], {
              cwd: repo,
              env: cacheEnv,
              timeout: 10_000,
            })
          ).stdout,
        );
        const seeded = await run(
          "go",
          [
            "run",
            "-tags",
            "fts5 sqlite_vec",
            "./scripts/meeting-fixture",
            archive,
          ],
          {
            cwd: repo,
            env: { ...env, ...caches, CGO_ENABLED: "1" },
            timeout: 120_000,
            maxBuffer: 256 * 1024,
          },
        );
        const manifest = JSON.parse(seeded.stdout) as SeedManifest;
        await writeFile(
          join(archive, "config.toml"),
          `[server]\napi_port = 0\nbind_addr = "127.0.0.1"\napi_key = "${apiKey}"\n[analytics]\nengine = "auto"\nauto_build_cache = false\n[vector]\nenabled = false\n`,
          { mode: 0o600 },
        );
        // make web-e2e builds this embedded binary before Playwright starts.
        let origin = "";
        const startDaemon = async () => {
          let startupLog = "";
          child = spawn(binary, ["--home", archive, "serve"], {
            cwd: repo,
            env,
            stdio: ["ignore", "pipe", "pipe"],
          });
          child.stdout?.on("data", (data: Buffer) => {
            capture(data);
            startupLog = (startupLog + data.toString()).slice(-64 * 1024);
          });
          child.stderr?.on("data", capture);
          let spawnError: Error | undefined;
          child.on("error", (error) => {
            spawnError = error;
          });
          await expect
            .poll(
              async () => {
                if (spawnError) throw spawnError;
                if (child!.exitCode !== null || child!.signalCode !== null)
                  throw new Error(
                    `Fixture daemon exited during startup:\n${logs}`,
                  );
                origin =
                  startupLog.match(
                    /API server: (http:\/\/127\.0\.0\.1:\d+)/,
                  )?.[1] ?? "";
                if (!origin) return false;
                try {
                  return (
                    await fetch(`${origin}/api/session`, {
                      signal: AbortSignal.timeout(1_000),
                      headers: { "User-Agent": userAgent },
                    })
                  ).ok;
                } catch {
                  return false;
                }
              },
              {
                timeout: 30_000,
                message: "Temporary real daemon becomes ready",
              },
            )
            .toBe(true);
        };
        await startDaemon();
        const post = (path: string, data: unknown) =>
          fetch(`${origin}/api/v1/${path}`, {
            method: "POST",
            headers: {
              Authorization: `Bearer ${apiKey}`,
              "Content-Type": "application/json",
              "User-Agent": userAgent,
            },
            body: JSON.stringify(data),
            signal: AbortSignal.timeout(30_000),
          });
        const imported = await post("import/meeting", {
          source: {
            identifier: "fixture-generic",
            display_name: "Synthetic generic meetings",
            account_email: "owner@example.org",
          },
          meeting: {
            external_id: "generic-unknown",
            title: "Generic unknown duration",
            started_at: "2026-02-04T10:00:00Z",
            organizer: { name: "Archive Owner", email: "owner@example.org" },
            attendees: [{ name: "Blair Example", email: "blair@example.net" }],
            transcript:
              "Generic transcript evidence without a summary or timing.",
            action_items: [],
          },
        });
        expect(imported.status, await imported.clone().text()).toBe(201);
        const generic = (await imported.json()) as MeetingRef;
        const promoted = await post("people", {
          participant_id: manifest.participant_id,
        });
        expect(promoted.ok, await promoted.clone().text()).toBe(true);
        const person = (await promoted.json()) as { id: number };
        const cache = await run(
          binary,
          ["--home", archive, "build-cache", "--full-rebuild"],
          { cwd: repo, env, timeout: 120_000, maxBuffer: 256 * 1024 },
        );
        logs += `\nCache build:\n${cache.stdout}${cache.stderr}`;
        // Startup chose SQL before a cache existed. A normal restart loads the
        // completed DuckDB cache for Explore and Relationships.
        if (!child || child.exitCode !== null || child.signalCode !== null) {
          throw new Error(`Import daemon exited unexpectedly:\n${logs}`);
        }
        await stop(child);
        logs += "\nRestarting fixture daemon with the completed cache.\n";
        await startDaemon();
        const manifestPath = testInfo.outputPath(
          "meeting-fixture-manifest.json",
        );
        await writeFile(
          manifestPath,
          JSON.stringify(
            {
              ...manifest,
              generic,
              personID: person.id,
              origin,
              daemonPID: child?.pid,
            },
            null,
            2,
          ),
        );
        await testInfo.attach("meeting-fixture-manifest", {
          path: manifestPath,
          contentType: "application/json",
        });
        await use({ ...manifest, origin, generic, personID: person.id, post });
        if (!child || child.exitCode !== null || child.signalCode !== null) {
          throw new Error(`Acceptance daemon exited unexpectedly:\n${logs}`);
        }
      } finally {
        try {
          if (child) await stop(child);
          const logPath = testInfo.outputPath("meeting-daemon.log");
          await writeFile(logPath, logs);
          await testInfo.attach("meeting-daemon-log", {
            path: logPath,
            contentType: "text/plain",
          });
        } finally {
          await rm(scratch, { recursive: true, force: true });
          await expect(access(scratch)).rejects.toThrow();
          await testInfo.attach("meeting-fixture-teardown", {
            body: JSON.stringify({ scratchRemoved: true, daemonPID: child?.pid, exitCode: child?.exitCode, signalCode: child?.signalCode }),
            contentType: "application/json",
          });
        }
      }
    },
    { timeout: 180_000 },
  ],
});

export { expect };
export function meetingURL(
  daemon: MeetingDaemon,
  state: Record<string, unknown> = {},
): string {
  return `${daemon.origin}/?explore=${encodeURIComponent(JSON.stringify({ workspace: "everything", filters: [{ dimension: "message_type", values: ["meeting_transcript"] }], ...state }))}`;
}

export async function loginToMeetingArchive(
  page: Page,
  daemon: MeetingDaemon,
): Promise<void> {
  await page.goto(meetingURL(daemon));
  await page
    .getByRole("textbox", { name: "API key", exact: true })
    .fill(apiKey);
  await page.getByRole("button", { name: "Log in", exact: true }).click();
  await expect(page.getByRole("main", { name: "Authentication" })).toHaveCount(
    0,
  );
}
