import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { loadMixedArchive } from './e2e/fixtures/mixed-archive';

export default async function prepareBrowserArchive() {
  // Filtered runs that do not use the mixed archive can skip its preparation.
  if (process.env.MSGVAULT_BROWSER_SKIP_ARCHIVE_PREPARE === '1') return;
  const root = mkdtempSync(join(tmpdir(), 'msgvault-browser-archive-'));
  const fixturePath = join(root, 'mixed-archive.json');
  try {
    const fixture = await loadMixedArchive();
    writeFileSync(fixturePath, JSON.stringify(fixture));
    process.env.MSGVAULT_BROWSER_MIXED_ARCHIVE_FIXTURE = fixturePath;
  } catch (error) {
    rmSync(root, { recursive: true, force: true });
    throw error;
  }
  return () => {
    delete process.env.MSGVAULT_BROWSER_MIXED_ARCHIVE_FIXTURE;
    rmSync(root, { recursive: true, force: true });
  };
}
