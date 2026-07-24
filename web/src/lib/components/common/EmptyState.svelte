<script lang="ts">
  interface Props {
    /** Hairline-stroke glyph matching the pane's subject matter. */
    glyph: 'conversations' | 'people' | 'envelope' | 'pulse' | 'search';
    label: string;
    hint?: string;
    /** Live-region role announced to assistive tech; panes that report a
     * transient state keep `status`, hard failures use `alert`. */
    role?: 'status' | 'alert' | 'presentation';
  }

  let { glyph, label, hint = undefined, role = 'status' }: Props = $props();
</script>

<div class="empty-state" role={role === 'presentation' ? undefined : role}>
  <svg
    class="empty-glyph"
    width="44"
    height="44"
    viewBox="0 0 48 48"
    fill="none"
    stroke="currentColor"
    stroke-width="1.25"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    {#if glyph === 'conversations'}
      <path d="M10 7h15a4 4 0 0 1 4 4v8a4 4 0 0 1-4 4h-8l-5 5v-5h-2a4 4 0 0 1-4-4v-8a4 4 0 0 1 4-4Z" />
      <path d="M33 18h1a4 4 0 0 1 4 4v8a4 4 0 0 1-4 4h-2v5l-5-5h-8a4 4 0 0 1-4-4v-1" />
    {:else if glyph === 'people'}
      <circle cx="18" cy="16" r="6" />
      <path d="M7 38a11 11 0 0 1 22 0" />
      <path d="M31 11.5a6 6 0 0 1 0 9" />
      <path d="M34 27.5a11 11 0 0 1 7 10.5" />
    {:else if glyph === 'envelope'}
      <path d="M8 21 24 9l16 12" />
      <path d="M8 21v17h32V21" />
      <path d="m8 21 16 11 16-11" />
    {:else if glyph === 'pulse'}
      <path d="M5 29h9l4-11 6 20 5-15 3 6h11" />
      <circle cx="24" cy="10" r="1.5" />
    {:else if glyph === 'search'}
      <circle cx="21" cy="21" r="11" />
      <path d="m29.5 29.5 11 11" />
      <path d="M15 21a6 6 0 0 1 6-6" />
    {/if}
  </svg>
  <p class="empty-label" data-section-label>{label}</p>
  {#if hint}<p class="empty-hint">{hint}</p>{/if}
</div>

<style>
  .empty-state {
    display: flex;
    flex: 1;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: var(--space-3);
    padding: var(--space-8) var(--space-5);
    text-align: center;
  }

  .empty-glyph {
    margin-bottom: var(--space-2);
    color: var(--text-muted);
    opacity: 0.6;
  }

  .empty-label {
    margin: 0;
  }

  .empty-hint {
    margin: 0;
    max-width: 340px;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }
</style>
