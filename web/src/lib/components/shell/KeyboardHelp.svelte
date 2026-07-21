<script lang="ts">
  import { KbdBadge, Modal, SearchInput } from '@kenn-io/kit-ui';

  import type { AppCommand } from '../../commands/registry';

  let { commands, onclose }: { commands: AppCommand[]; onclose: () => void } = $props();
  let query = $state('');
  const shortcuts = $derived(commands.filter((command) => command.keys.length > 0));
  const filtered = $derived.by(() => {
    const needle = query.trim().toLowerCase();
    return needle
      ? shortcuts.filter((command) => `${command.label} ${command.keywords}`.toLowerCase().includes(needle))
      : shortcuts;
  });
  const sections = $derived([...new Set(filtered.map(({ section }) => section))]);
</script>

<Modal title="Keyboard shortcuts" ariaLabel="Keyboard shortcuts" width="min(680px, calc(100vw - 32px))" {onclose}>
  <div class="keyboard-help-search">
    <SearchInput
      value={query}
      ariaLabel="Search keyboard shortcuts"
      placeholder="Search shortcuts"
      block
      autofocus
      oninput={(value) => { query = value; }}
    />
  </div>
  <div class="keyboard-help-list" aria-live="polite">
    {#if filtered.length === 0}
      <p>No matching shortcuts.</p>
    {/if}
    {#each sections as section (section)}
      <section aria-labelledby={`shortcut-${section.toLowerCase()}`}>
        <h2 id={`shortcut-${section.toLowerCase()}`}>{section}</h2>
        <dl>
          {#each filtered.filter((command) => command.section === section) as command (command.id)}
            <div>
              <dt>{command.label}</dt>
              <dd>
                {#if command.combos.length === 1 && command.keys.length > 1}
                  <KbdBadge keys={[...command.keys]} />
                {:else}
                  {#each command.keys as key, index (key)}
                    {#if index > 0}<span aria-hidden="true">or</span>{/if}
                    <KbdBadge keys={[key]} />
                  {/each}
                {/if}
                {#if command.destructive}<span class="review-note">opens review</span>{/if}
              </dd>
            </div>
          {/each}
        </dl>
      </section>
    {/each}
  </div>
</Modal>

<style>
  .keyboard-help-search {
    position: sticky;
    top: 0;
    z-index: 1;
    padding-bottom: var(--space-4);
    background: var(--bg-surface);
  }

  .keyboard-help-list {
    display: grid;
    gap: var(--space-5);
  }

  h2 {
    margin: 0 0 var(--space-2);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    letter-spacing: 0.08em;
    text-transform: uppercase;
  }

  dl {
    display: grid;
    margin: 0;
  }

  dl div {
    display: grid;
    min-height: var(--row-height);
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-4);
    border-top: 1px solid var(--border-muted);
  }

  dt {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  dd {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .review-note {
    color: var(--status-destructive-ink);
  }
</style>
