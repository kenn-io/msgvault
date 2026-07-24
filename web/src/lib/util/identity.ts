/** Deterministic identity presentation helpers: stable hash-derived hues and
 * display initials for avatar glyphs. Presentation-only — never serialized
 * into exports or APIs. */

/** FNV-1a 32-bit hash of a seed string, folded to a hue in [0, 360). */
export function identityHue(seed: string): number {
  let hash = 0x811c9dc5;
  for (let index = 0; index < seed.length; index += 1) {
    hash ^= seed.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193);
  }
  return (hash >>> 0) % 360;
}

/** 1-2 uppercase initials for a display label.
 *
 * Email-like labels use the local part ("jane.doe@example.com" → "JD");
 * multi-word names use the first character of the first two words; anything
 * else falls back to its first character.
 */
export function initialsFor(label: string): string {
  const trimmed = label.trim();
  if (trimmed === '') return '?';
  const at = trimmed.indexOf('@');
  const base = at > 0 ? trimmed.slice(0, at) : trimmed;
  const initials = base
    .split(/[\s._-]+/)
    .map((word) => word.match(/[\p{L}\p{N}]/u)?.[0])
    .filter((char): char is string => char !== undefined);
  if (initials.length === 0) return [...trimmed][0]!.toUpperCase();
  return initials.slice(0, 2).join('').toUpperCase();
}
