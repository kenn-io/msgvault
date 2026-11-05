<script lang="ts">
  import { Button, EmptyState, formatShortcutKeys, KbdBadge, Modal, SearchInput } from '@kenn-io/kit-ui';

  import type { AppCommand } from '../../commands/registry';

  let { commands, onclose, onOpenCommands }: { commands: AppCommand[]; onclose: () => void; onOpenCommands: (query: string) => void } = $props();
  let query = $state('');
  const shortcuts = $derived(commands.filter((command) => command.keys.length > 0));
  const palette = $derived(commands.find(({ id }) => id === 'open-command-palette'));
  const filtered = $derived.by(() => {
    const needle = query.trim().toLowerCase();
    return needle
      ? shortcuts.filter((command) => `${command.label} ${command.keywords}`.toLowerCase().includes(needle))
      : shortcuts;
  });
  const sections = $derived([...new Set(filtered.map(({ section }) => section))]);
  const tracks = $derived([
    sections.filter((section) => section === 'Navigate'),
    sections.filter((section) => section !== 'Navigate')
  ]);
</script>

<div class="keyboard-help">
  <Modal title="Keyboard shortcuts" ariaLabel="Keyboard shortcuts" width="var(--keyboard-help-width)" maxWidth="calc(100vw - 32px)" maxHeight="76svh" closable {onclose}>
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
        <EmptyState title={`No shortcuts match "${query.trim()}"`}>
          <Button label="Search commands instead" onclick={() => onOpenCommands(query)} />
        </EmptyState>
      {:else}
        {#each tracks as track, index (index)}
          <div class="keyboard-help-track">
            {#each track as section (section)}
              <section aria-labelledby={`shortcut-${section.toLowerCase()}`}>
                <h2 id={`shortcut-${section.toLowerCase()}`}>{section}</h2>
                <dl>
                  {#each filtered.filter((command) => command.section === section) as command (command.id)}
                    <div>
                      <dt>{command.label}</dt>
                      <dd>
                        {#each command.keys as combo, index (combo)}
                          {#if index > 0}<span aria-hidden="true">or</span>{/if}
                          <KbdBadge keys={formatShortcutKeys(combo)} />
                        {/each}
                        {#if command.destructive}<span class="review-note">opens review first</span>{/if}
                      </dd>
                    </div>
                  {/each}
                </dl>
              </section>
            {/each}
          </div>
        {/each}
      {/if}
    </div>
    {#snippet footer()}
      <Button label="Open command palette" class="keyboard-help-commands" onclick={() => onOpenCommands('')}>
        {#snippet trailing()}{#each palette?.keys ?? [] as combo (combo)}<KbdBadge keys={formatShortcutKeys(combo)} />{/each}{/snippet}
      </Button>
    {/snippet}
  </Modal>
</div>

<style>
  .keyboard-help {
    --keyboard-help-width: 680px;
  }

  .keyboard-help :global(.kit-modal-overlay) {
    align-items: flex-start;
    padding-top: 12svh;
  }

  .keyboard-help-search {
    padding-bottom: 16px;
    flex-shrink: 0;
  }

  .keyboard-help :global(.kit-modal-body) {
    display: flex;
    flex-direction: column;
    min-height: 0;
    overflow: hidden;
  }

  .keyboard-help-list {
    height: min(520px, calc(76svh - 160px));
    min-height: 0;
    overflow-y: auto;
  }

  .keyboard-help-track:empty {
    display: none;
  }

  .keyboard-help-list :global(.kit-empty-state__title) {
    overflow-wrap: anywhere;
  }

  .keyboard-help-track:not(:empty) + .keyboard-help-track,
  section + section {
    margin-top: 16px;
  }

  h2 {
    margin: 0 0 var(--space-2);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-semibold, 600);
  }

  dl {
    display: grid;
    margin: 0;
  }

  dl div {
    display: grid;
    min-height: 32px;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-4);
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
    color: var(--text-muted);
  }

  .keyboard-help :global(.keyboard-help-commands) {
    border-color: transparent;
    background: transparent;
    box-shadow: none;
  }

  /* kit-ui-check-ignore: two shortcut columns need room for labels and review notes. */
  @media (min-width: 1200px) {
    .keyboard-help {
      --keyboard-help-width: 880px;
    }

    .keyboard-help-list {
      display: grid;
      grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
      column-gap: 32px;
      align-content: start;
    }

    .keyboard-help-list > .keyboard-help-track + .keyboard-help-track {
      margin-top: 0;
    }

    .keyboard-help-list > :global(.kit-empty-state) {
      grid-column: 1 / -1;
    }
  }
</style>
