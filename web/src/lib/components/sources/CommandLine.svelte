<script lang="ts">
  import { CopyButton } from '@kenn-io/kit-ui';

  /** A command the user runs in a terminal, shown with a copy button. */
  let { command }: { command: string } = $props();
</script>

<span class="command">
  <code>{command}</code>
  {#key command}<CopyButton text={command} ariaLabel={`Copy ${command}`} />{/key}
</span>

<style>
  .command {
    display: flex;
    box-sizing: border-box;
    width: 100%;
    min-width: 0;
    max-width: 100%;
    min-height: var(--control-h);
    align-items: center;
    gap: var(--control-gap);
    padding: var(--space-2) var(--space-2) var(--space-2) var(--pane-pad-x);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }
  code {
    min-width: 0;
    flex: 1;
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    user-select: all;
  }
</style>
