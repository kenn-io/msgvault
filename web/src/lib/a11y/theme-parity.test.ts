import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const root = process.cwd();

// Strip comments before any block/declaration matching below: a comment
// immediately preceding a selector (e.g. `/* note */\n:root`) would
// otherwise be captured into the selector text by the block-matching regex,
// and a comment inside a declaration's value could otherwise defeat the
// value-shape check.
function stripCSSComments(css: string): string {
  return css.replace(/\/\*[\s\S]*?\*\//g, '');
}

function definedTokenNames(cssPath: string): string[] {
  const css = stripCSSComments(readFileSync(join(root, cssPath), 'utf8'));
  return [...new Set([...css.matchAll(/--[\w-]+(?=\s*:)/g)].map((match) => match[0]))].sort();
}

// Heuristic: a custom property counts as a "colour token" if its declared
// value looks like a literal colour (hex, rgb()/rgba(), hsl()/hsla(),
// oklch(), or color-mix()) OR its name matches the naming convention the
// theme files already use for colour roles (--bg-*, --text-*, --border-*,
// --control-*, --selected-*, --status-*, --accent-*, plus the handful of
// one-off ink/overlay/focus names below). Matching on both value shape and
// name catches tokens that reference another custom property (no literal
// colour in the value) as well as tokens whose value is a literal colour.
const COLOR_VALUE_PATTERN = /(#[0-9a-f]{3,8}\b|\brgba?\(|\bhsla?\(|\boklch\(|\bcolor-mix\()/i;
const COLOR_NAME_PATTERN =
  /^--(bg|text|border|control|selected|status|accent)-|^--(active-ink|link-ink|focus-color|artifact-ink|overlay-bg|status-waiting)$/;

function unscopedRootColorTokensFromCSS(css: string): Array<{ name: string; value: string }> {
  const found: Array<{ name: string; value: string }> = [];
  for (const block of stripCSSComments(css).matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const selectors = block[1]!.split(',').map((selector) => selector.trim());
    if (!selectors.includes(':root')) continue;
    // CSS allows the last declaration in a block to omit its trailing
    // semicolon; the block's own captured content never includes the
    // closing brace, so append one if missing rather than silently skipping
    // that declaration.
    const body = block[2]!.trim().replace(/;?$/, ';');
    for (const declaration of body.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) {
      const [, name, value] = declaration as unknown as [string, string, string];
      if (COLOR_VALUE_PATTERN.test(value) || COLOR_NAME_PATTERN.test(name)) {
        found.push({ name, value: value.trim() });
      }
    }
  }
  return found;
}

function unscopedRootColorTokens(cssPath: string): Array<{ name: string; value: string }> {
  return unscopedRootColorTokensFromCSS(readFileSync(join(root, cssPath), 'utf8'));
}

describe('theme token parity', () => {
  it('defines the identical custom-property set in light and dark themes', () => {
    expect(definedTokenNames('src/styles/theme-dark.css'))
      .toEqual(definedTokenNames('src/styles/theme-light.css'));
  });

  it('overrides color-scheme for dark mode', () => {
    const tokens = readFileSync(join(root, 'src/styles/tokens.css'), 'utf8');
    expect(tokens).toMatch(/:root\[data-theme='dark'\]\s*\{[^}]*color-scheme:\s*dark/);
  });

  it('declares no colour token on an unscoped :root block', () => {
    // A bare `:root` selector applies regardless of `data-theme`, so any
    // colour token declared there silently leaks its value into every theme
    // that forgets to override it. Colour tokens must live only in
    // `:root[data-theme='light']` / `:root[data-theme='dark']` (or a shared
    // selector like `:root:not([data-theme='dark'])`), never on a bare
    // `:root` block.
    for (const file of ['src/styles/theme-light.css', 'src/styles/theme-dark.css', 'src/styles/tokens.css']) {
      expect(unscopedRootColorTokens(file), file).toEqual([]);
    }
  });

  it('catches an unscoped colour token whose :root selector is preceded by a comment', () => {
    // The block-matching regex captures everything up to the opening brace
    // into the selector group, including a leading comment — without
    // stripping comments first, `selectors.includes(':root')` never matches
    // a trimmed string like `/* note */\n:root`.
    const css = '/* leaked token */\n:root {\n  --bg-surface: #ffffff;\n}\n';
    expect(unscopedRootColorTokensFromCSS(css)).toEqual([{ name: '--bg-surface', value: '#ffffff' }]);
  });

  it('catches an unscoped colour token that is the last declaration with no trailing semicolon', () => {
    // CSS allows the final declaration in a block to omit the semicolon;
    // the declaration-matching regex requires one, so without normalizing
    // the block's own captured text first, this declaration is silently
    // skipped rather than flagged.
    const css = ':root {\n  --text-primary: #000000\n}\n';
    expect(unscopedRootColorTokensFromCSS(css)).toEqual([{ name: '--text-primary', value: '#000000' }]);
  });

  it('ignores a colour-shaped value inside a comment on an unscoped :root block', () => {
    const css = ':root {\n  /* was #ff0000 */\n  --accent-primary: #00ff00;\n}\n';
    expect(unscopedRootColorTokensFromCSS(css)).toEqual([{ name: '--accent-primary', value: '#00ff00' }]);
  });
});
