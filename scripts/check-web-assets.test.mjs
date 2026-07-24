import assert from 'node:assert/strict';
import { cpSync, mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { resolve } from 'node:path';
import test from 'node:test';

import { validateWebAssets } from './check-web-assets.mjs';

const root = resolve(import.meta.dirname, '..');

function releaseCopy(t) {
  const dir = mkdtempSync(resolve(tmpdir(), 'msgvault-assets-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const dist = resolve(dir, 'dist');
  const embedded = resolve(dir, 'embedded');
  cpSync(resolve(root, 'web/dist'), dist, { recursive: true });
  cpSync(resolve(root, 'web/dist'), embedded, { recursive: true });
  return { dist, embedded };
}

function releaseEntry(paths, extension) {
  const manifest = JSON.parse(readFileSync(resolve(paths.dist, '.vite/manifest.json')));
  const candidates = [manifest['index.html'].file, ...(manifest['index.html'].css ?? [])];
  return candidates.find((path) => path.endsWith(extension));
}

function writeReleaseFile(paths, logicalPath, contents) {
  for (const rootPath of [paths.dist, paths.embedded]) {
    const path = resolve(rootPath, logicalPath);
    mkdirSync(resolve(path, '..'), { recursive: true });
    writeFileSync(path, contents);
  }
}

function appendReleaseFile(paths, logicalPath, contents) {
  const original = readFileSync(resolve(paths.dist, logicalPath), 'utf8');
  writeReleaseFile(paths, logicalPath, `${original}\n${contents}\n`);
}

test('validates the production Vite graph and staged embed', (t) => {
  const paths = releaseCopy(t);
  const result = validateWebAssets(paths);
  assert.ok(result.files.some((file) => file.path === 'index.html'));
  assert.ok(result.files.some((file) => file.path.endsWith('.mjs')));
});

test('rejects missing and escaping release references', (t) => {
  const missing = releaseCopy(t);
  const missingManifest = JSON.parse(readFileSync(resolve(missing.dist, '.vite/manifest.json')));
  rmSync(resolve(missing.dist, missingManifest['index.html'].file));
  assert.throws(() => validateWebAssets(missing), /release asset is missing/);

  const escaping = releaseCopy(t);
  writeFileSync(resolve(escaping.dist, 'index.html'), '<script src="../escape.js"></script>');
  assert.throws(() => validateWebAssets(escaping), /escapes the release root/);
});

test('rejects source maps, external scripts, fonts, and embed drift', (t) => {
  const sourceMap = releaseCopy(t);
  const manifestPath = resolve(sourceMap.dist, '.vite/manifest.json');
  const manifest = JSON.parse(readFileSync(manifestPath));
  manifest['index.html'].assets = [...(manifest['index.html'].assets ?? []), 'assets/index.js.map'];
  mkdirSync(resolve(sourceMap.dist, 'assets'), { recursive: true });
  writeFileSync(resolve(sourceMap.dist, 'assets/index.js.map'), '{}');
  writeFileSync(manifestPath, JSON.stringify(manifest));
  assert.throws(() => validateWebAssets(sourceMap), /source map/);

  const externalScript = releaseCopy(t);
  writeFileSync(resolve(externalScript.dist, 'index.html'), '<script src="https://cdn.example.invalid/app.js"></script>');
  assert.throws(() => validateWebAssets(externalScript), /absolute external URL/);

  const externalFont = releaseCopy(t);
  const css = JSON.parse(readFileSync(resolve(externalFont.dist, '.vite/manifest.json')))['index.html'].css[0];
  writeFileSync(resolve(externalFont.dist, css), '@font-face{src:url(https://cdn.example.invalid/font.woff2)}');
  cpSync(resolve(externalFont.dist, css), resolve(externalFont.embedded, css));
  assert.throws(() => validateWebAssets(externalFont), /external font\/asset URL/);

  const drift = releaseCopy(t);
  writeFileSync(resolve(drift.embedded, 'index.html'), '<!doctype html><title>drift</title>');
  assert.throws(() => validateWebAssets(drift), /embedded asset differs/);

  const extra = releaseCopy(t);
  writeFileSync(resolve(extra.dist, 'assets/untracked.txt'), 'not in the release graph');
  assert.throws(() => validateWebAssets(extra), /untracked release asset/);
});

test('walks nested CSS and JavaScript references from the real distribution', (t) => {
  const paths = releaseCopy(t);
  const css = releaseEntry(paths, '.css');
  const js = releaseEntry(paths, '.js');
  assert.ok(css);
  assert.ok(js);

  appendReleaseFile(paths, css, '@import "./theme-A1B2C3D4.css";');
  writeReleaseFile(paths, 'assets/theme-A1B2C3D4.css', '.nested{background:url("./tile-E5F6G7H8.png")}');
  writeReleaseFile(paths, 'assets/tile-E5F6G7H8.png', 'synthetic image bytes');
  appendReleaseFile(paths, js, 'import("./runtime-I9J0K1L2.js");');
  writeReleaseFile(paths, 'assets/runtime-I9J0K1L2.js', 'new URL("./worker-M3N4O5P6.mjs", import.meta.url);');
  writeReleaseFile(paths, 'assets/worker-M3N4O5P6.mjs', 'export default "worker";');

  const result = validateWebAssets(paths);
  for (const expected of [
    'assets/theme-A1B2C3D4.css',
    'assets/tile-E5F6G7H8.png',
    'assets/runtime-I9J0K1L2.js',
    'assets/worker-M3N4O5P6.mjs'
  ]) assert.ok(result.files.some(({ path }) => path === expected), expected);
});

test('rejects missing, escaping, mapped, and external nested CSS references', (t) => {
  const missing = releaseCopy(t);
  appendReleaseFile(missing, releaseEntry(missing, '.css'), '.missing{background:url("./missing-A1B2C3D4.png")}');
  assert.throws(() => validateWebAssets(missing), /release asset is missing/);

  const escaping = releaseCopy(t);
  appendReleaseFile(escaping, releaseEntry(escaping, '.css'), '@import "../../escape-A1B2C3D4.css";');
  assert.throws(() => validateWebAssets(escaping), /escapes the release root/);

  const mapped = releaseCopy(t);
  appendReleaseFile(mapped, releaseEntry(mapped, '.css'), '/*# sourceMappingURL=./index-A1B2C3D4.css.map */');
  assert.throws(() => validateWebAssets(mapped), /source map/);

  const external = releaseCopy(t);
  appendReleaseFile(external, releaseEntry(external, '.css'), '@import "https://cdn.example.invalid/theme.css";');
  assert.throws(() => validateWebAssets(external), /external font\/asset URL/);
});

test('rejects missing, escaping, mapped, and external nested JavaScript references', (t) => {
  const missing = releaseCopy(t);
  appendReleaseFile(missing, releaseEntry(missing, '.js'), 'import("./missing-A1B2C3D4.js");');
  assert.throws(() => validateWebAssets(missing), /release asset is missing/);

  const escaping = releaseCopy(t);
  appendReleaseFile(escaping, releaseEntry(escaping, '.js'), 'import("../../escape-A1B2C3D4.js");');
  assert.throws(() => validateWebAssets(escaping), /escapes the release root/);

  const mapped = releaseCopy(t);
  appendReleaseFile(mapped, releaseEntry(mapped, '.js'), '//# sourceMappingURL=./index-A1B2C3D4.js.map');
  assert.throws(() => validateWebAssets(mapped), /source map/);

  const external = releaseCopy(t);
  appendReleaseFile(external, releaseEntry(external, '.js'), 'import "https://cdn.example.invalid/runtime.js";');
  assert.throws(() => validateWebAssets(external), /external runtime asset/);
});

test('rejects hidden and credential-pattern files anywhere in the shipped trees', (t) => {
  const hidden = releaseCopy(t);
  writeReleaseFile(hidden, '.env', 'MSGVAULT_SECRET=1');
  assert.throws(() => validateWebAssets(hidden), /hidden file or directory/);

  const nestedHidden = releaseCopy(t);
  writeReleaseFile(nestedHidden, 'assets/.credentials/token.json', '{}');
  assert.throws(() => validateWebAssets(nestedHidden), /hidden file or directory/);

  for (const name of ['client_secret_web.json', 'oauth_client_prod.json', 'server.pem', 'private.key', 'config.toml']) {
    const credential = releaseCopy(t);
    writeReleaseFile(credential, name, 'synthetic credential bytes');
    assert.throws(() => validateWebAssets(credential), /credential filename pattern/, name);
  }

  const embeddedOnlyCredential = releaseCopy(t);
  writeFileSync(resolve(embeddedOnlyCredential.embedded, 'client_secret.json'), '{}');
  assert.throws(() => validateWebAssets(embeddedOnlyCredential), /credential filename pattern/);

  const embeddedOnlyStray = releaseCopy(t);
  writeFileSync(resolve(embeddedOnlyStray.embedded, 'notes.txt'), 'stray file');
  assert.throws(() => validateWebAssets(embeddedOnlyStray), /untracked embedded asset/);

  const stub = releaseCopy(t);
  writeFileSync(resolve(stub.embedded, 'stub.html'), 'ok\n');
  validateWebAssets(stub);
});

test('requires immutable release assets to carry Vite hash-bearing names', (t) => {
  const paths = releaseCopy(t);
  const manifestPath = resolve(paths.dist, '.vite/manifest.json');
  const manifest = JSON.parse(readFileSync(manifestPath));
  manifest['index.html'].assets = [...(manifest['index.html'].assets ?? []), 'assets/unhashed.css'];
  writeReleaseFile(paths, 'assets/unhashed.css', '.unhashed{color:inherit}');
  writeFileSync(manifestPath, JSON.stringify(manifest));
  writeFileSync(resolve(paths.embedded, '.vite/manifest.json'), JSON.stringify(manifest));
  assert.throws(() => validateWebAssets(paths), /immutable release asset lacks a content hash/);
});
