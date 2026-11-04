import { appShortcuts, escapeCloses } from '@kenn-io/kit-ui';

export function claimDialogEscape(scope: string, onclose: () => void): () => void {
  const popScope = appShortcuts.pushScope(scope);
  const close = escapeCloses(onclose);
  const handleEscape = (event: KeyboardEvent): void => {
    if (appShortcuts.activeScope() !== scope) return;
    const panel = [...document.querySelectorAll('[aria-modal="true"]')].at(-1);
    // kit-ui-check-ignore: observes Kit popup roles; renders no popup markup.
    if (panel?.querySelector('[role="listbox"], [role="menu"], [role="dialog"]:not([aria-modal="true"])')) return;
    close(event);
  };
  document.addEventListener('keydown', handleEscape);
  return () => { document.removeEventListener('keydown', handleEscape); popScope(); };
}
