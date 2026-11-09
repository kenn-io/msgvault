<script lang="ts">
  import { MenuItem, Toggle } from '@kenn-io/kit-ui';

  interface Props {
    label: string;
    checked: boolean;
    /** The line under the label when nothing is in flight, such as "Since Nov 6". */
    status?: string;
    /** A save is in flight: the line reads "Turning on…" or "Turning off…". */
    busy?: boolean;
    /** The state is being read; the item waits. */
    loading?: boolean;
    /** What failed, as "Couldn't save"; the item then reads "… · Retry" and selecting it retries. */
    error?: string;
    onretry?: () => void;
    /** The feature needs setup first: the item reads "Set up…" and opens it. */
    onsetup?: () => void;
    onchange: (checked: boolean) => void;
  }

  let { label, checked, status = '', busy = false, loading = false, error = '', onretry, onsetup, onchange }: Props = $props();
  const secondary = $derived(onsetup ? 'Set up…' : busy ? (checked ? 'Turning off…' : 'Turning on…') : error ? `${error} · Retry` : status);
  // MenuItem owns keyboard navigation; the switch is its visual state. The attachment reruns as `checked` changes.
  function semantics(node: HTMLElement): void {
    const item = node.closest('button');
    item?.setAttribute('role', 'menuitemcheckbox');
    item?.setAttribute('aria-checked', String(checked));
  }
</script>

<MenuItem closeOnSelect={false} disabled={busy || (loading && !onsetup)} textValue={label} onselect={() => {
  if (onsetup) onsetup();
  else if (error && onretry) onretry();
  else onchange(!checked);
}}>
  <span class="copy menu-switch-copy" {@attach semantics}><span>{label}</span>{#if secondary}<small>{secondary}</small>{/if}</span>
  {#snippet trailing()}{#if !onsetup}<span class="switch" inert aria-hidden="true"><Toggle {checked} ariaLabel={label} disabled={busy || loading || !!error} /></span>{/if}{/snippet}
</MenuItem>

<style>
  /* One line uses the menu row; two lines use 54 (Body over Meta, 8 above and below), with the switch on the first line. */
  :global(:root:root .kit-menu__item:has(.menu-switch-copy)) { align-items: center; gap: var(--space-group); min-height: var(--list-item-h); padding-block: 0; }
  :global(:root:root .kit-menu__item:has(.menu-switch-copy) .kit-menu__trailing) { display: flex; }
  .copy { display: grid; white-space: normal; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  :global(:root:root .kit-menu__item:has(.menu-switch-copy small)) { align-items: flex-start; min-height: var(--list-item-h-2); padding-block: var(--space-group); }
  small { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .switch { display: inline-flex; align-items: center; height: var(--type-body-line); pointer-events: none; }
</style>
