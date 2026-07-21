#!/usr/bin/env node

import { createHash } from 'node:crypto';
import { existsSync, lstatSync, readFileSync, readdirSync, realpathSync, writeFileSync } from 'node:fs';
import { dirname, isAbsolute, posix, relative, resolve, sep } from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const webRequire = createRequire(resolve(repositoryRoot, 'web/package.json'));

function loadHTMLParser() {
  try {
    return webRequire('jsdom').JSDOM;
  } catch (error) {
    throw new Error(`jsdom is unavailable; run 'make web-install' first (${error.message})`);
  }
}

function loadAssetParsers() {
  try {
    const lexer = webRequire('es-module-lexer');
    lexer.initSync();
    return { parseJavaScript: lexer.parse, postcss: webRequire('postcss') };
  } catch (error) {
    throw new Error(`asset parsers are unavailable; run 'make web-install' first (${error.message})`);
  }
}

function sha256(bytes) {
  return createHash('sha256').update(bytes).digest('hex');
}

function assertContained(root, candidate, label) {
  const rel = relative(realpathSync(root), realpathSync(candidate));
  if (rel === '..' || rel.startsWith(`..${sep}`) || isAbsolute(rel)) {
    throw new Error(`${label} escapes ${root}: ${candidate}`);
  }
}

function releaseReference(value, containingPath, label, externalDescription = 'absolute external URL') {
  const reference = value.trim();
  if (!reference) throw new Error(`${label} is empty`);
  if (/^(?:[a-z][a-z0-9+.-]*:|\/\/)/i.test(reference)) {
    throw new Error(`${label} uses an ${externalDescription}: ${reference}`);
  }
  let clean;
  try {
    clean = decodeURIComponent(reference.split(/[?#]/, 1)[0]);
  } catch {
    throw new Error(`${label} has invalid URL encoding: ${reference}`);
  }
  if (!clean || clean.includes('\\')) {
    throw new Error(`${label} escapes the release root: ${reference}`);
  }
  const logical = clean.startsWith('/')
    ? posix.normalize(clean.slice(1))
    : posix.normalize(posix.join(posix.dirname(containingPath), clean));
  if (!logical || logical === '..' || logical.startsWith('../') || posix.isAbsolute(logical)) {
    throw new Error(`${label} escapes the release root: ${reference}`);
  }
  return logical;
}

function rootReference(value, label) {
  return releaseReference(value, 'index.html', label);
}

function assertImmutableName(logicalPath) {
  if (!logicalPath.startsWith('assets/')) return;
  if (!/-[A-Za-z0-9_-]{8}(?:\.[A-Za-z0-9]+)+$/.test(posix.basename(logicalPath))) {
    throw new Error(`immutable release asset lacks a content hash: ${logicalPath}`);
  }
}

function manifestGraph(manifest, entryKey = 'index.html') {
  const reachable = new Set(['index.html', '.vite/manifest.json']);
  const visiting = new Set();

  function visit(key) {
    if (visiting.has(key)) return;
    const entry = manifest[key];
    if (!entry) throw new Error(`Vite manifest references missing entry ${key}`);
    visiting.add(key);
    for (const field of [entry.file, ...(entry.css ?? []), ...(entry.assets ?? [])]) {
      if (typeof field !== 'string') throw new Error(`Vite manifest entry ${key} has a non-string asset`);
      reachable.add(rootReference(field, `manifest ${key}`));
    }
    for (const dependency of [...(entry.imports ?? []), ...(entry.dynamicImports ?? [])]) visit(dependency);
  }

  visit(entryKey);
  return reachable;
}

function sourceMapReferences(text) {
  return [...text.matchAll(/[#@]\s*sourceMappingURL\s*=\s*([^\s*]+)/gi)].map((match) => match[1]);
}

function cssURLReferences(value) {
  return [...value.matchAll(/url\(\s*(?:(['"])(.*?)\1|([^)'"\s]+))\s*\)/gi)]
    .map((match) => match[2] ?? match[3]);
}

function cssImportReference(params) {
  const match = params.match(/^\s*(?:url\(\s*(?:(['"])(.*?)\1|([^)'"\s]+))\s*\)|(['"])(.*?)\4|([^\s;]+))/i);
  return match ? (match[2] ?? match[3] ?? match[5] ?? match[6]) : undefined;
}

function ignorableReference(value) {
  return /^(?:data:|blob:|#)/i.test(value.trim());
}

function inspectReferencedFile(path, logicalPath, enqueue, parsers, allowSourceMaps) {
  if (!/\.(?:css|js|mjs)$/i.test(logicalPath)) return;
  const text = readFileSync(path, 'utf8');
  for (const value of sourceMapReferences(text)) {
    if (!allowSourceMaps) throw new Error(`source map is present in release graph: ${value}`);
    enqueue(value, logicalPath, `${logicalPath} source map`);
  }
  if (/\.css$/i.test(logicalPath)) {
    const root = parsers.postcss.parse(text, { from: logicalPath });
    root.walkDecls((declaration) => {
      for (const value of cssURLReferences(declaration.value)) {
        if (!ignorableReference(value)) {
          enqueue(value, logicalPath, `${logicalPath} url()`, 'external font/asset URL');
        }
      }
    });
    root.walkAtRules('import', (rule) => {
      const value = cssImportReference(rule.params);
      if (value && !ignorableReference(value)) {
        enqueue(value, logicalPath, `${logicalPath} @import`, 'external font/asset URL');
      }
    });
    return;
  }

  const [imports] = parsers.parseJavaScript(text, logicalPath);
  for (const imported of imports) {
    const value = imported.n;
    if (typeof value !== 'string' || ignorableReference(value)) continue;
    if (/^(?:[a-z][a-z0-9+.-]*:|\/\/)/i.test(value)) {
      throw new Error(`${logicalPath} uses an external runtime asset: ${value}`);
    }
    if (!/^(?:\.{0,2}\/)/.test(value)) {
      throw new Error(`${logicalPath} uses a non-local runtime asset: ${value}`);
    }
    enqueue(value, logicalPath, `${logicalPath} import`, 'external runtime asset');
  }
  for (const match of text.matchAll(/new\s+URL\s*\(\s*(['"])([^'"]+)\1\s*,\s*import\.meta\.url\s*\)/g)) {
    const value = match[2];
    if (!ignorableReference(value)) {
      enqueue(value, logicalPath, `${logicalPath} new URL()`, 'external runtime asset');
    }
  }
}

function distributionFiles(root, directory = root) {
  const files = [];
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const path = resolve(directory, entry.name);
    const logical = relative(root, path).split(sep).join('/');
    if (entry.isSymbolicLink()) throw new Error(`release output contains a symlink: ${logical}`);
    if (entry.isDirectory()) files.push(...distributionFiles(root, path));
    else if (entry.isFile()) files.push(logical);
    else throw new Error(`release output contains an unsupported filesystem entry: ${logical}`);
  }
  return files;
}

export function validateWebAssets(options = {}) {
  const dist = resolve(options.dist ?? resolve(repositoryRoot, 'web/dist'));
  const embedded = resolve(options.embedded ?? resolve(repositoryRoot, 'internal/web/dist'));
  const allowSourceMaps = options.allowSourceMaps ?? process.env.MSGVAULT_ALLOW_WEB_SOURCEMAPS === '1';
  const indexPath = resolve(dist, 'index.html');
  const manifestPath = resolve(dist, '.vite/manifest.json');
  if (!existsSync(indexPath) || !existsSync(manifestPath)) {
    throw new Error(`release web output is incomplete under ${dist}; run 'make web-build'`);
  }

  const JSDOM = loadHTMLParser();
  const parsers = loadAssetParsers();
  const document = new JSDOM(readFileSync(indexPath, 'utf8')).window.document;
  const references = new Set();
  const queue = [];
  function enqueue(value, containingPath = 'index.html', label = containingPath, externalDescription) {
    const logicalPath = releaseReference(value, containingPath, label, externalDescription);
    if (!allowSourceMaps && /\.map$/i.test(logicalPath)) {
      throw new Error(`source map is present in release graph: ${logicalPath}`);
    }
    assertImmutableName(logicalPath);
    if (!references.has(logicalPath)) {
      references.add(logicalPath);
      queue.push(logicalPath);
    }
  }
  for (const element of document.querySelectorAll('script[src], link[href]')) {
    const attr = element.tagName === 'SCRIPT' ? 'src' : 'href';
    enqueue(element.getAttribute(attr), 'index.html', `index.html ${element.tagName.toLowerCase()}[${attr}]`);
  }

  const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
  for (const reference of manifestGraph(manifest)) enqueue(reference);
  // Vite copies this compliance artifact from web/public. It is intentionally
  // distributed even though application code does not request it at runtime.
  enqueue('licenses/Figtree-OFL.txt');

  const files = [];
  for (let index = 0; index < queue.length; index += 1) {
    const reference = queue[index];
    if (!allowSourceMaps && /\.map$/i.test(reference)) {
      throw new Error(`source map is present in release graph: ${reference}`);
    }
    const path = resolve(dist, reference);
    if (!existsSync(path) || !lstatSync(path).isFile()) throw new Error(`release asset is missing: ${reference}`);
    assertContained(dist, path, `release asset ${reference}`);
    inspectReferencedFile(path, reference, enqueue, parsers, allowSourceMaps);
    const bytes = readFileSync(path);
    files.push({ path: reference, bytes: bytes.length, sha256: sha256(bytes) });
  }
  files.sort((left, right) => left.path.localeCompare(right.path));
  const recorded = new Set(files.map((file) => file.path));
  const untracked = distributionFiles(dist).filter((file) => !recorded.has(file));
  if (untracked.length > 0) throw new Error(`untracked release asset: ${untracked.join(', ')}`);

  if (options.checkEmbedded !== false) {
    for (const file of files) {
      const embeddedPath = resolve(embedded, file.path);
      if (!existsSync(embeddedPath) || !lstatSync(embeddedPath).isFile()) {
        throw new Error(`embedded asset is missing: ${file.path}`);
      }
      assertContained(embedded, embeddedPath, `embedded asset ${file.path}`);
      const bytes = readFileSync(embeddedPath);
      if (sha256(bytes) !== file.sha256) throw new Error(`embedded asset differs from release build: ${file.path}`);
    }
  }

  if (options.binary) {
    const binary = readFileSync(resolve(options.binary));
    for (const file of files) {
      const bytes = readFileSync(resolve(dist, file.path));
      if (binary.indexOf(bytes) < 0) throw new Error(`binary does not embed release asset bytes: ${file.path}`);
    }
  }

  return { schema: 1, files };
}

function parseArgs(argv) {
  const options = {};
  let list = false;
  let jsonPath = '';
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === '--dist') options.dist = argv[++i];
    else if (arg === '--embedded') options.embedded = argv[++i];
    else if (arg === '--binary') options.binary = argv[++i];
    else if (arg === '--no-embedded') options.checkEmbedded = false;
    else if (arg === '--allow-source-maps') options.allowSourceMaps = true;
    else if (arg === '--list') list = true;
    else if (arg === '--json') jsonPath = argv[++i];
    else throw new Error(`unknown argument: ${arg}`);
  }
  return { options, list, jsonPath };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const { options, list, jsonPath } = parseArgs(process.argv.slice(2));
    const result = validateWebAssets(options);
    const canonical = `${JSON.stringify(result, null, 2)}\n`;
    if (jsonPath) writeFileSync(resolve(jsonPath), canonical);
    if (list) for (const file of result.files) process.stdout.write(`${file.path}\n`);
    else process.stdout.write(`validated ${result.files.length} release web assets\n`);
  } catch (error) {
    process.stderr.write(`web asset validation failed: ${error.message}\n`);
    process.exitCode = 1;
  }
}
