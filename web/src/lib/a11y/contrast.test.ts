import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { basename, dirname, extname, join, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

type Theme = 'light' | 'dark';
type Pair = { foreground: string; background: string; source: string; threshold: number };

const root = process.cwd();
const sourceFiles = collect(join(root, 'src')).filter((path) => ['.css', '.svelte'].includes(extname(path)));
const kitComponentFiles = discoverKitComponents(sourceFiles);
const componentCSS = [...sourceFiles, ...kitComponentFiles]
  .flatMap((path) => cssBlocks(path).map((css) => ({ path, css })));
const sharedCSS = [
  readFileSync(join(root, 'node_modules/@kenn-io/kit-ui/src/lib/theme.css'), 'utf8'),
  readFileSync(join(root, 'src/styles/tokens.css'), 'utf8'),
  readFileSync(join(root, 'src/styles/density.css'), 'utf8')
].join('\n');

describe.each<Theme>(['light', 'dark'])('%s rendered semantic color graph', (theme) => {
  const graph = parseTokens(`${sharedCSS}\n${readFileSync(join(root, `src/styles/theme-${theme}.css`), 'utf8')}`);

  it('loads every consumed kit-ui component style block into the rendered graph', () => {
    const kitDirectory = join(root, 'node_modules/@kenn-io/kit-ui/src/lib/components');
    const discovered = new Set(componentCSS.filter(({ path }) => path.startsWith(kitDirectory)).map(({ path }) => path));

    expect(kitComponentFiles.length).toBeGreaterThan(0);
    expect(kitComponentFiles.map((path) => basename(path))).toEqual(expect.arrayContaining(['Button.svelte', 'CommandPalette.svelte']));
    expect([...discovered].sort()).toEqual(kitComponentFiles.filter((path) => cssBlocks(path).length > 0).sort());
  });

  it('includes styles from known transitive kit-ui component dependencies', () => {
    expect(kitComponentFiles.map((path) => basename(path))).toEqual(expect.arrayContaining([
      'SearchInput.svelte',
      'TextInput.svelte',
      'Modal.svelte',
      'IconButton.svelte'
    ]));
  });

  it('defines every semantic variable consumed by application CSS', () => {
    const missing = new Set<string>();
    for (const { css } of componentCSS) {
      for (const declaration of declarations(css)) {
        if (!isSemanticProperty(declaration.property)) continue;
        for (const reference of variableReferences(declaration.value)) {
          if (!graph.has(reference.name) && !reference.hasFallback) missing.add(reference.name);
        }
      }
    }
    expect([...missing].sort()).toEqual([]);
  });

  it('meets contrast for every rendered component-local semantic pair', () => {
    const failures = renderedPairs(graph).flatMap((pair) => {
      const background = resolveColor(graph, pair.background);
      const ratio = contrast(resolveColor(graph, pair.foreground, new Set(), background), background);
      return ratio + 1e-9 < pair.threshold
        ? [`${pair.source}: ${pair.foreground} on ${pair.background} = ${ratio.toFixed(2)}:1 < ${pair.threshold}:1`]
        : [];
    });
    expect(failures).toEqual([]);
  });

  it('treats inset box-shadow color layers as structural indicators only', () => {
    const pairs = renderedPairs(graph);
    expect(pairs.some(({ source }) => source.includes('FilesWorkspace.svelte .files-grid:focus-visible'))).toBe(true);
    expect(pairs.some(({ source }) => source.includes('EverythingTable.svelte .data-row--selected'))).toBe(true);
    expect(pairs).toEqual(expect.arrayContaining([
      expect.objectContaining({
        source: expect.stringContaining('EverythingTable.svelte .data-row--selected'),
        foreground: 'var(--text-secondary)'
      })
    ]));
    expect(pairs.some(({ foreground }) => foreground === 'var(--shadow-md)')).toBe(false);
  });
});

describe('density token authority', () => {
  const graph = parseTokens(sharedCSS);

  it('keeps the CSS-owned row heights within the approved ranges', () => {
    const css = readFileSync(join(root, 'src/styles/density.css'), 'utf8');
    const compact = parseRule(css, ':root[data-density="compact"]');
    const comfortable = parseRule(css, ':root[data-density="comfortable"]');
    expect(px(compact, '--row-height')).toBeGreaterThanOrEqual(34);
    expect(px(compact, '--row-height')).toBeLessThanOrEqual(38);
    expect(px(comfortable, '--row-height')).toBeGreaterThanOrEqual(44);
    expect(px(comfortable, '--row-height')).toBeLessThanOrEqual(48);
    expect(graph.has('--row-height')).toBe(true);
  });
});

describe('Settings semantic styling', () => {
  it('uses theme roles instead of light-only raw colors', () => {
    const css = cssBlocks(join(root, 'src/lib/components/settings/SettingsWorkspace.svelte')).join('\n');
    expect(css.match(/#[0-9a-f]{3,8}\b/gi) ?? []).toEqual([]);
  });
});

function renderedPairs(graph: Map<string, string>): Pair[] {
  const pairs = new Map<string, Pair>();
  const add = (foreground: string, background: string, source: string, threshold: number) => {
    if (foreground === background) return;
    if ([...variables(foreground), ...variables(background)].some((token) => !graph.has(token))) return;
    pairs.set(`${foreground}\0${background}\0${source}\0${threshold}`, { foreground, background, source, threshold });
  };
  for (const { path, css } of componentCSS) {
    const parsedRules = rules(css);
    for (const rule of parsedRules) {
      const ownForegrounds = rule.declarations
        .filter(({ property }) => property === 'color')
        .map(({ value }) => colorExpression(value))
        .filter(isColorExpression);
      const backgrounds = rule.declarations
        .filter(({ property }) => property === 'background' || property === 'background-color')
        .map(({ value }) => backgroundExpression(value))
        .filter(isColorExpression);
      const boundaries = rule.declarations
        .filter(({ property }) => isBoundaryProperty(property))
        .map(({ value }) => colorExpression(value))
        .filter(isColorExpression);
      const shadowIndicators = rule.declarations
        .filter(({ property }) => property === 'box-shadow')
        .flatMap(({ value }) => structuralShadowColors(value));
      const surfaces = backgrounds.length > 0 ? backgrounds : ['var(--bg-primary)', 'var(--bg-surface)'];
      const foregrounds = ownForegrounds.length > 0
        ? ownForegrounds
        : backgrounds.length > 0 ? inheritedForegrounds(rule.selector, parsedRules) : [];
      for (const foreground of foregrounds) for (const background of surfaces) {
        add(foreground, background, `${relative(path)} ${rule.selector}`, 4.5);
      }
      if (isStructuralSelector(rule.selector)) {
        for (const boundary of boundaries) for (const background of surfaces) {
          add(boundary, background, `${relative(path)} ${rule.selector}`, 3);
        }
      }
      for (const indicator of shadowIndicators) for (const background of surfaces) {
        add(indicator, background, `${relative(path)} ${rule.selector}`, 3);
      }
    }
  }
  for (const [name] of graph) {
    const match = /^(--status-[\w-]+)-ink$/.exec(name);
    if (match && graph.has(`${match[1]}-bg`)) {
      add(`var(${name})`, `var(${match[1]}-bg)`, 'semantic status recipe', 4.5);
    }
  }
  return [...pairs.values()];
}

function inheritedForegrounds(
  selector: string,
  parsedRules: ReturnType<typeof rules>
): string[] {
  const bases = new Set<string>();
  for (const part of selector.split(',').map((value) => value.trim())) {
    const withoutPseudo = part.replace(/:{1,2}[\w-]+(?:\([^)]*\))?/g, '');
    bases.add(withoutPseudo);
    for (const match of withoutPseudo.matchAll(/\.([\w-]+)--[\w-]+/g)) {
      bases.add(withoutPseudo.replace(match[0], `.${match[1]}`));
    }
  }
  const inherited: string[] = [];
  for (const candidate of parsedRules) {
    if (!candidate.selector.split(',').map((value) => value.trim()).some((value) => bases.has(value))) continue;
    inherited.push(...candidate.declarations
      .filter(({ property }) => property === 'color')
      .map(({ value }) => colorExpression(value))
      .filter(isColorExpression));
  }
  return inherited;
}

function isStructuralSelector(selector: string): boolean {
  return /(?::focus(?:-visible)?|--selected\b|\.selected\b|\.highlighted\b|\[aria-(?:current|selected)=)/.test(selector);
}

function collect(directory: string): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    return entry.isDirectory() ? collect(path) : [path];
  });
}

function discoverKitComponents(paths: string[]): string[] {
  const kitDirectory = join(root, 'node_modules/@kenn-io/kit-ui/src/lib');
  const entry = join(kitDirectory, 'index.ts');
  const exports = exportedKitModules(entry, kitDirectory);
  const roots = new Set<string>();
  for (const path of paths) {
    const contents = readFileSync(path, 'utf8');
    for (const match of contents.matchAll(/import\s*\{([\s\S]*?)\}\s*from\s*['"]@kenn-io\/kit-ui['"]/g)) {
      for (const imported of match[1]!.split(',')) {
        const name = imported.trim().replace(/^type\s+/, '').split(/\s+as\s+/)[0];
        if (!name) continue;
        const module = exports.get(name);
        if (module) roots.add(module);
      }
    }
  }

  const visited = new Set<string>();
  const components = new Set<string>();
  const visit = (path: string): void => {
    if (visited.has(path)) return;
    visited.add(path);
    if (extname(path) === '.svelte') components.add(path);
    const contents = readFileSync(path, 'utf8');
    for (const specifier of moduleSpecifiers(contents)) {
      const dependency = resolveKitModule(specifier, path, kitDirectory);
      if (dependency) visit(dependency);
    }
  };
  for (const path of roots) visit(path);
  return [...components].sort();
}

function exportedKitModules(entry: string, kitDirectory: string): Map<string, string> {
  const modules = new Map<string, string>();
  const contents = readFileSync(entry, 'utf8');
  for (const match of contents.matchAll(/export\s+(?:type\s+)?\{([\s\S]*?)\}\s*from\s*['"]([^'"]+)['"]/g)) {
    const module = resolveKitModule(match[2]!, entry, kitDirectory);
    if (!module) continue;
    for (const binding of match[1]!.split(',')) {
      const parts = binding.trim().replace(/^type\s+/, '').split(/\s+as\s+/);
      const exported = parts[1] ?? parts[0];
      if (exported) modules.set(exported, module);
    }
  }
  for (const match of contents.matchAll(/export\s*\*\s*from\s*['"]([^'"]+)['"]/g)) {
    const barrel = resolveKitModule(match[1]!, entry, kitDirectory);
    if (!barrel) continue;
    for (const [name, module] of exportedKitModules(barrel, kitDirectory)) modules.set(name, module);
  }
  return modules;
}

function moduleSpecifiers(contents: string): string[] {
  return [...contents.matchAll(/(?:import|export)\s+(?:type\s+)?(?:[\s\S]*?\s+from\s+)?['"]([^'"]+)['"]/g)]
    .map((match) => match[1]!);
}

function resolveKitModule(specifier: string, importer: string, kitDirectory: string): string | undefined {
  if (!specifier.startsWith('.')) return undefined;
  const unresolved = resolve(dirname(importer), specifier);
  if (!unresolved.startsWith(`${kitDirectory}/`) && unresolved !== kitDirectory) return undefined;
  const candidates = [
    unresolved,
    unresolved.replace(/\.js$/, '.ts'),
    `${unresolved}.svelte`,
    `${unresolved}.ts`,
    `${unresolved}.js`,
    join(unresolved, 'index.ts'),
    join(unresolved, 'index.js')
  ];
  return candidates.find((candidate) => existsSync(candidate));
}

function cssBlocks(path: string): string[] {
  const contents = readFileSync(path, 'utf8');
  if (extname(path) === '.css') return [contents];
  return [...contents.matchAll(/<style(?:\s[^>]*)?>([\s\S]*?)<\/style>/g)].map((match) => match[1]!);
}

function rules(css: string): Array<{ selector: string; declarations: ReturnType<typeof declarations> }> {
  const plain = css.replace(/\/\*[\s\S]*?\*\//g, '');
  return [...plain.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((match) => ({
    selector: match[1]!.trim().replace(/\s+/g, ' '),
    declarations: declarations(match[2]!)
  }));
}

function declarations(css: string): Array<{ property: string; value: string }> {
  return [...css.matchAll(/([\w-]+)\s*:\s*([^;{}]+);/g)].map((match) => ({
    property: match[1]!.toLowerCase(), value: match[2]!.trim()
  }));
}

function isSemanticProperty(property: string): boolean {
  return /^(color|background(?:-color)?|fill|stroke|box-shadow)$/.test(property) || isBoundaryProperty(property);
}

function isBoundaryProperty(property: string): boolean {
  return /^(border|border-color|border-(?:top|right|bottom|left)(?:-color)?|outline|outline-color)$/.test(property);
}

function variables(value: string): string[] {
  return variableReferences(value).map(({ name }) => name);
}

function variableReferences(value: string): Array<{ name: string; hasFallback: boolean }> {
  return [...value.matchAll(/var\((--[\w-]+)/g)].map((match) => {
    let depth = 1;
    let hasFallback = false;
    const start = match.index! + match[0].length;
    for (let index = start; index < value.length && depth > 0; index += 1) {
      if (value[index] === '(') depth += 1;
      else if (value[index] === ')') depth -= 1;
      else if (value[index] === ',' && depth === 1) hasFallback = true;
    }
    return { name: match[1]!, hasFallback };
  });
}

function parseTokens(css: string): Map<string, string> {
  const tokens = new Map<string, string>();
  for (const match of css.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) tokens.set(match[1]!, match[2]!.trim());
  return tokens;
}

function parseRule(css: string, selector: string): Map<string, string> {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const match = new RegExp(`${escaped}\\s*\\{([^}]+)\\}`).exec(css);
  expect(match, `missing ${selector}`).not.toBeNull();
  return parseTokens(match![1]!);
}

function colorExpression(value: string): string {
  const mix = value.match(/color-mix\([\s\S]+\)/)?.[0];
  if (mix) return mix;
  if (/^var\([\s\S]+\)$/.test(value.trim())) return value.trim();
  const token = variables(value).at(-1);
  if (token) return `var(${token})`;
  return value.trim();
}

function backgroundExpression(value: string): string {
  return colorExpression(splitTopLevel(value, ',').at(-1) ?? value);
}

// Only inset layers are component boundaries/focus/selection indicators.
// Non-inset shadow recipes provide elevation and are intentionally excluded
// from WCAG non-text contrast pairing against the element surface.
function structuralShadowColors(value: string): string[] {
  return splitTopLevel(value, ',')
    .filter((layer) => /\binset\b/.test(layer))
    .map(colorExpression)
    .filter(isColorExpression);
}

function isColorExpression(value: string): boolean {
  return /^(?:var\(--|color-mix\(|rgb\(|#[0-9a-f]{3}(?:[0-9a-f]{3})?\b)/i.test(value);
}

function splitTopLevel(value: string, separator: string): string[] {
  const parts: string[] = [];
  let depth = 0;
  let start = 0;
  for (let index = 0; index < value.length; index += 1) {
    if (value[index] === '(') depth += 1;
    else if (value[index] === ')') depth -= 1;
    else if (value[index] === separator && depth === 0) {
      parts.push(value.slice(start, index).trim());
      start = index + 1;
    }
  }
  parts.push(value.slice(start).trim());
  return parts;
}

function resolveColor(
  tokens: Map<string, string>, expression: string, seen = new Set<string>(), underlay?: string
): string {
  if (expression.trim() === 'transparent') return underlay ?? resolveColor(tokens, 'var(--bg-surface)', seen);
  const variable = /^var\((--[\w-]+)(?:\s*,\s*([\s\S]+))?\)$/.exec(expression.trim());
  if (variable) {
    const name = variable[1]!;
    if (seen.has(name)) throw new Error(`cyclic token ${name}`);
    seen.add(name);
    const value = tokens.get(name);
    if (!value) {
      if (variable[2]) return resolveColor(tokens, variable[2], seen, underlay);
      throw new Error(`missing token ${name}`);
    }
    return resolveColor(tokens, colorExpression(value), seen, underlay);
  }
  const mixHeader = /^color-mix\(\s*in srgb\s*,/i.exec(expression.trim());
  if (mixHeader && expression.trim().endsWith(')')) {
    const parts = splitTopLevel(expression.trim().slice(mixHeader[0].length, -1), ',');
    if (parts.length !== 2) throw new Error(`expected two color-mix operands, got ${expression}`);
    const operands = parts.map((part) => {
      const weighted = /^(.*?)(?:\s+(\d+(?:\.\d+)?)%)?$/.exec(part)!;
      return { color: weighted[1]!.trim(), weight: weighted[2] ? Number(weighted[2]) / 100 : undefined };
    });
    const left = rgb(resolveColor(tokens, operands[0]!.color, new Set(seen), underlay));
    const right = rgb(resolveColor(tokens, operands[1]!.color, new Set(seen), underlay));
    const leftWeight = operands[0]!.weight ?? (operands[1]!.weight === undefined ? 0.5 : 1 - operands[1]!.weight);
    const rightWeight = operands[1]!.weight ?? 1 - leftWeight;
    return hex(left.map((channel, index) => channel * leftWeight + right[index]! * rightWeight));
  }
  const functional = /^rgb\(\s*(\d+(?:\.\d+)?)\s+(\d+(?:\.\d+)?)\s+(\d+(?:\.\d+)?)(?:\s*\/\s*(\d+(?:\.\d+)?)%)?\s*\)$/i.exec(expression.trim());
  if (functional) {
    const channels = functional.slice(1, 4).map(Number);
    const alpha = functional[4] === undefined ? 1 : Number(functional[4]) / 100;
    if (alpha === 1) return hex(channels);
    const base = rgb(underlay ?? resolveColor(tokens, 'var(--bg-surface)', new Set(seen)));
    return hex(channels.map((channel, index) => channel * alpha + base[index]! * (1 - alpha)));
  }
  if (/^#[0-9a-f]{3}$/i.test(expression)) {
    return `#${[...expression.slice(1)].map((digit) => `${digit}${digit}`).join('')}`;
  }
  if (!/^#[0-9a-f]{6}$/i.test(expression)) {
    throw new Error(`expected color expression, got ${expression}`);
  }
  return expression;
}

function px(tokens: Map<string, string>, name: string): number {
  const value = tokens.get(name);
  expect(value).toMatch(/^\d+(?:\.\d+)?px$/);
  return Number.parseFloat(value!);
}

function contrast(foreground: string, background: string): number {
  const values = [luminance(foreground), luminance(background)].sort((a, b) => b - a);
  return (values[0]! + 0.05) / (values[1]! + 0.05);
}

function luminance(color: string): number {
  const channels = [1, 3, 5].map((offset) => Number.parseInt(color.slice(offset, offset + 2), 16) / 255);
  const [r, g, b] = channels.map((channel) => channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4);
  return 0.2126 * r! + 0.7152 * g! + 0.0722 * b!;
}

function rgb(color: string): number[] {
  return [1, 3, 5].map((offset) => Number.parseInt(color.slice(offset, offset + 2), 16));
}

function hex(channels: number[]): string {
  return `#${channels.map((channel) => Math.round(channel).toString(16).padStart(2, '0')).join('')}`;
}

function relative(path: string): string {
  return path.slice(root.length + 1);
}
