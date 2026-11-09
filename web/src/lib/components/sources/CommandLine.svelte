<script lang="ts">
  import { Button, copyToClipboard } from '@kenn-io/kit-ui';
  import { onDestroy } from 'svelte';

  /** A command the user runs in a terminal, with Copy, which reads "Copied" for two seconds once it has. */
  let { command }: { command: string } = $props();
  let copiedCommand = $state<string>();
  let reset: ReturnType<typeof setTimeout> | undefined;
  // A changed command reads Copy again.
  const copied = $derived(copiedCommand === command);

  async function copy(): Promise<void> {
    const text = command;
    if (!(await copyToClipboard(text))) return;
    copiedCommand = text;
    clearTimeout(reset);
    reset = setTimeout(() => (copiedCommand = undefined), 2000);
  }
  onDestroy(() => clearTimeout(reset));
</script>

<span class="command">
  <code>{command}</code>
  <Button size="sm" label={copied ? 'Copied' : 'Copy'} ariaLabel={copied ? 'Copied' : `Copy ${command}`} onclick={() => void copy()} />
</span>

<style>
  /* A 36 row: the command inset 12, and Copy (28) centered on it. A long command
   * wraps and the row grows with it, Copy staying on the first line. */
  .command {
    display: flex;
    box-sizing: border-box;
    width: 100%;
    min-width: 0;
    min-height: 36px;
    align-items: flex-start;
    gap: var(--space-group);
    padding: calc((36px - var(--control-h)) / 2 - var(--border-width, 1px));
    padding-left: var(--space-box);
    border: var(--border-width, 1px) solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }
  code {
    flex: 1;
    min-width: 0;
    padding-block: calc((var(--control-h) - var(--type-meta-line)) / 2);
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-family: var(--font-mono);
    font-size: var(--type-meta-size);
    line-height: var(--type-meta-line);
    user-select: all;
  }
  .command :global(.kit-button) { flex: none; }
</style>
