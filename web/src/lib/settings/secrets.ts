/** The shortest value that gets a hint: six of twelve characters leaves as much hidden as shown. */
const HINT_MINIMUM_LENGTH = 12;

/**
 * Masks a secret for display the way the daemon does: "sk-…x9Q", the first
 * three and last three characters. A value too short to hide most of itself
 * gets no hint. Used for a key typed in the browser that is not saved yet;
 * saved keys carry the daemon's own hint.
 */
export function maskSecret(value: string): string {
  const chars = [...value];
  if (chars.length < HINT_MINIMUM_LENGTH) return '';
  return `${chars.slice(0, 3).join('')}…${chars.slice(-3).join('')}`;
}

/** Show a pending replacement or removal before the saved secret state. */
export function pendingSecret(saved: { configured: boolean; hint?: string } | undefined, pending: { action: 'set'; value: string } | { action: 'clear' } | undefined): { configured: boolean; hint: string } {
  if (pending?.action === 'set') return { configured: true, hint: maskSecret(pending.value) };
  if (pending?.action === 'clear') return { configured: false, hint: '' };
  return { configured: saved?.configured ?? false, hint: saved?.hint ?? '' };
}
