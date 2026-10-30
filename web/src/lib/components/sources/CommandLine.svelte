<script lang="ts">
  import { CopyButton } from '@kenn-io/kit-ui';

  /** A command the user runs in a terminal, shown with a copy button. */
  let { command }: { command: string } = $props();
</script>

<span class="command">
  <code>{command}</code>
  <CopyButton text={command} ariaLabel={`Copy ${command}`} />
</span>

<style>
  .command {
    display: inline-flex;
    max-width: 100%;
    min-height: var(--control-h);
    align-items: center;
    gap: var(--control-gap);
    padding: 0 var(--space-2) 0 var(--pane-pad-x);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }
  code {
    min-width: 0;
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    user-select: all;
  }
</style>
