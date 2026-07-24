/**
 * Conservative allowlist for author inline styles in archived mail.
 *
 * Declarations are parsed with a strict grammar and re-serialized; anything
 * not provably inert is dropped. There is no url()/image/position/animation
 * surface: the only parentheses that survive are rgb()/rgba()/hsl()/hsla()
 * with numeric arguments, values may not contain quotes, escapes, colons, or
 * control characters, and `!important` is stripped while the declaration is
 * kept. Negative margins are allowed only in px down to -8px — enough for the
 * small optical adjustments real mail uses, never enough to overlay content
 * outside the message. `<style>` blocks stay stripped entirely (scoping and
 * specificity risks outweigh their value); this inline allowlist plus the
 * legacy presentational attributes covers the vast majority of real mail.
 */

type ValueCheck = (tokens: string[], value: string) => boolean;

const HEX_COLOR = /^#(?:[0-9a-f]{3,4}|[0-9a-f]{6}|[0-9a-f]{8})$/i;
const COLOR_FUNCTION = /^(?:rgb|rgba|hsl|hsla)\([\d.%\s,/+-]+\)$/i;
const COLOR_FUNCTION_GLOBAL = /(?:rgb|rgba|hsl|hsla)\([\d.%\s,/+-]+\)/gi;
const IDENT = /^[a-z][a-z-]*$/i;
const LENGTH = /^[+-]?(?:\d+\.?\d*|\.\d+)(?:px|em|rem|ex|ch|pt|pc|cm|mm|in|%)$/i;
const ZERO = /^[+-]?0+(?:\.0*)?$/;
const NUMBER = /^\d+(?:\.\d+)?$/;
const NEGATIVE_PX = /^-(\d+\.?\d*|\.\d+)px$/i;
const FONT_FAMILY = /^[a-z0-9][a-z0-9 ,-]*$/i;
const REJECTED_VALUE_CHARACTERS = /[\\"'`;:<>{}@!*=?[\]&^~$|\u0000-\u001f\u007f-\u009f]/;
const MAX_VALUE_LENGTH = 256;
const MAX_NEGATIVE_MARGIN_PX = 8;

const FONT_SIZE_KEYWORDS = new Set([
  'xx-small', 'x-small', 'small', 'medium', 'large', 'x-large', 'xx-large', 'smaller', 'larger'
]);
const BORDER_STYLES = new Set([
  'none', 'hidden', 'dotted', 'dashed', 'solid', 'double', 'groove', 'ridge', 'inset', 'outset'
]);
const BORDER_WIDTH_KEYWORDS = new Set(['thin', 'medium', 'thick']);
const DECORATION_LINES = new Set(['none', 'underline', 'overline', 'line-through']);
const VERTICAL_ALIGN_KEYWORDS = new Set([
  'baseline', 'top', 'middle', 'bottom', 'text-top', 'text-bottom', 'sub', 'super'
]);
const DISPLAY_VALUES = [
  'none', 'block', 'inline', 'inline-block', 'table', 'inline-table', 'table-row', 'table-cell',
  'table-row-group', 'table-header-group', 'table-footer-group', 'table-column',
  'table-column-group', 'table-caption'
];

function isColor(token: string): boolean {
  return HEX_COLOR.test(token) || COLOR_FUNCTION.test(token) || IDENT.test(token);
}

function isNonNegativeLength(token: string): boolean {
  return ZERO.test(token) || (LENGTH.test(token) && !token.startsWith('-'));
}

function isMarginToken(token: string): boolean {
  if (token.toLowerCase() === 'auto' || isNonNegativeLength(token)) return true;
  const negative = NEGATIVE_PX.exec(token);
  return negative?.[1] !== undefined && Number.parseFloat(negative[1]) <= MAX_NEGATIVE_MARGIN_PX;
}

function isBorderWidth(token: string): boolean {
  return isNonNegativeLength(token) || BORDER_WIDTH_KEYWORDS.has(token.toLowerCase());
}

function isBorderToken(token: string): boolean {
  return isNonNegativeLength(token) || isColor(token);
}

function isFontSize(token: string): boolean {
  return isNonNegativeLength(token) || FONT_SIZE_KEYWORDS.has(token.toLowerCase());
}

function isFontWeight(token: string): boolean {
  if (['normal', 'bold', 'bolder', 'lighter'].includes(token.toLowerCase())) return true;
  if (!NUMBER.test(token)) return false;
  const weight = Number.parseFloat(token);
  return weight >= 1 && weight <= 1000;
}

function isLineHeight(token: string): boolean {
  return token.toLowerCase() === 'normal' || NUMBER.test(token) || isNonNegativeLength(token);
}

function isLetterSpacing(token: string): boolean {
  return token.toLowerCase() === 'normal' || ZERO.test(token) || LENGTH.test(token);
}

function isVerticalAlign(token: string): boolean {
  return VERTICAL_ALIGN_KEYWORDS.has(token.toLowerCase()) || ZERO.test(token) ||
    LENGTH.test(token);
}

function isDecorationToken(token: string): boolean {
  return DECORATION_LINES.has(token.toLowerCase()) || isColor(token);
}

function single(check: (token: string) => boolean): ValueCheck {
  return (tokens) => tokens.length === 1 && tokens[0] !== undefined && check(tokens[0]);
}

function upTo(max: number, check: (token: string) => boolean): ValueCheck {
  return (tokens) => tokens.length >= 1 && tokens.length <= max && tokens.every(check);
}

function keyword(...allowed: string[]): ValueCheck {
  const set = new Set(allowed);
  return single((token) => set.has(token.toLowerCase()));
}

function buildValidators(): Map<string, ValueCheck> {
  const map = new Map<string, ValueCheck>();
  const color = single(isColor);
  map.set('color', color);
  map.set('background-color', color);
  // The background shorthand survives only when it reduces to a plain color.
  map.set('background', color);
  map.set('font-family', (_tokens, value) => FONT_FAMILY.test(value));
  map.set('font-size', single(isFontSize));
  map.set('font-weight', single(isFontWeight));
  map.set('font-style', keyword('normal', 'italic', 'oblique'));
  map.set('text-align', keyword('left', 'right', 'center', 'justify', 'start', 'end'));
  map.set('text-decoration', upTo(3, isDecorationToken));
  map.set('text-decoration-line', upTo(3, (token) => DECORATION_LINES.has(token.toLowerCase())));
  map.set('text-transform', keyword('none', 'uppercase', 'lowercase', 'capitalize', 'full-width'));
  map.set('line-height', single(isLineHeight));
  map.set('letter-spacing', single(isLetterSpacing));
  map.set('vertical-align', single(isVerticalAlign));
  map.set('white-space', keyword('normal', 'nowrap', 'pre', 'pre-wrap', 'pre-line', 'break-spaces'));
  map.set('border-collapse', keyword('collapse', 'separate'));
  map.set('border-spacing', upTo(2, isNonNegativeLength));
  map.set('table-layout', keyword('auto', 'fixed'));
  map.set('float', keyword('left', 'right', 'none'));
  map.set('clear', keyword('left', 'right', 'both', 'none'));
  map.set('direction', keyword('ltr', 'rtl'));
  map.set('display', keyword(...DISPLAY_VALUES));
  map.set('padding', upTo(4, isNonNegativeLength));
  map.set('margin', upTo(4, isMarginToken));
  map.set('width', single(isNonNegativeLength));
  map.set('height', single(isNonNegativeLength));
  map.set('max-width', single(isNonNegativeLength));
  map.set('min-width', single(isNonNegativeLength));
  map.set('border', upTo(3, isBorderToken));
  map.set('border-width', upTo(4, isBorderWidth));
  map.set('border-style', upTo(4, (token) => BORDER_STYLES.has(token.toLowerCase())));
  map.set('border-color', upTo(4, isColor));
  map.set('border-radius', upTo(4, isNonNegativeLength));
  for (const side of ['top', 'right', 'bottom', 'left']) {
    map.set(`padding-${side}`, single(isNonNegativeLength));
    map.set(`margin-${side}`, single(isMarginToken));
    map.set(`border-${side}`, upTo(3, isBorderToken));
    map.set(`border-${side}-width`, single(isBorderWidth));
    map.set(`border-${side}-style`, keyword(...BORDER_STYLES));
    map.set(`border-${side}-color`, single(isColor));
  }
  for (const corner of ['top-left', 'top-right', 'bottom-left', 'bottom-right']) {
    map.set(`border-${corner}-radius`, upTo(2, isNonNegativeLength));
  }
  return map;
}

const VALIDATORS = buildValidators();

/**
 * Normalizes one declaration value: collapses whitespace, strips a trailing
 * `!important`, then rejects anything outside the inert grammar (escapes,
 * quotes, colons, control characters, or parentheses that are not simple
 * numeric color functions).
 */
function normalizeValue(raw: string): string | undefined {
  let value = raw.replace(/\s+/g, ' ').trim();
  value = value.replace(/\s*!\s*important$/i, '').trim();
  if (value === '' || value.length > MAX_VALUE_LENGTH) return undefined;
  if (REJECTED_VALUE_CHARACTERS.test(value)) return undefined;
  if (/[()]/.test(value.replace(COLOR_FUNCTION_GLOBAL, ' '))) return undefined;
  return value;
}

function tokenize(value: string): string[] {
  return value.match(/(?:rgb|rgba|hsl|hsla)\([^()]*\)|[^\s()]+/gi) ?? [];
}

/**
 * Filters a sender-controlled `style` attribute down to allowlisted,
 * provably-inert declarations and re-serializes them. Returns an empty string
 * when nothing survives.
 */
export function sanitizeInlineStyle(style: string): string {
  const kept: string[] = [];
  const withoutComments = style.replace(/\/\*[\s\S]*?\*\//g, ' ');
  for (const declaration of withoutComments.split(';')) {
    const colon = declaration.indexOf(':');
    if (colon === -1) continue;
    const property = declaration.slice(0, colon).trim().toLowerCase();
    const check = VALIDATORS.get(property);
    if (!check) continue;
    const value = normalizeValue(declaration.slice(colon + 1));
    if (value === undefined || !check(tokenize(value), value)) continue;
    kept.push(`${property}: ${value}`);
  }
  return kept.join('; ');
}
