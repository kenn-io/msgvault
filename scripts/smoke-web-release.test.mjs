import assert from 'node:assert/strict';
import { mkdtempSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';
import test from 'node:test';

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');

function delay(milliseconds) {
  return new Promise((resolveDelay) => setTimeout(resolveDelay, milliseconds));
}

async function waitForScratch(parent, child) {
  for (let attempt = 0; attempt < 400; attempt += 1) {
    const scratch = readdirSync(parent).find((entry) => entry.startsWith('msgvault-web-release.'));
    if (scratch) return resolve(parent, scratch);
    if (child.exitCode !== null || child.signalCode !== null) break;
    await delay(5);
  }
  throw new Error('smoke process did not create its isolated scratch directory');
}

async function interruptSmoke(signal, expectedCode) {
  const scratchParent = mkdtempSync(resolve(tmpdir(), 'msgvault-smoke-signal-'));
  try {
    const child = spawn('bash', ['scripts/smoke-web-release.sh'], {
      cwd: repositoryRoot,
      env: { ...process.env, TMPDIR: scratchParent },
      stdio: ['ignore', 'pipe', 'pipe']
    });
    let stdout = '';
    let stderr = '';
    child.stdout.setEncoding('utf8');
    child.stderr.setEncoding('utf8');
    child.stdout.on('data', (chunk) => { stdout += chunk; });
    child.stderr.on('data', (chunk) => { stderr += chunk; });

    await waitForScratch(scratchParent, child);
    await delay(25);
    assert.equal(child.kill(signal), true, `send ${signal}`);
    const result = await new Promise((resolveClose, reject) => {
      const timeout = setTimeout(() => {
        child.kill('SIGKILL');
        reject(new Error(`${signal} smoke did not exit; stdout=${stdout}; stderr=${stderr}`));
      }, 30_000);
      child.once('close', (code, exitSignal) => {
        clearTimeout(timeout);
        resolveClose({ code, exitSignal });
      });
    });

    assert.deepEqual(result, { code: expectedCode, exitSignal: null }, `${signal}: ${stderr}`);
    assert.deepEqual(
      readdirSync(scratchParent).filter((entry) => entry.startsWith('msgvault-web-release.')),
      [],
      `${signal} left smoke scratch state`
    );
  } finally {
    rmSync(scratchParent, { recursive: true, force: true });
  }
}

test('INT and TERM preserve conventional nonzero statuses and remove scratch state', async () => {
  await interruptSmoke('SIGINT', 130);
  await interruptSmoke('SIGTERM', 143);
});
