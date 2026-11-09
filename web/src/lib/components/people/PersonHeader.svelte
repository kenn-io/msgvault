<script lang="ts">
  import type { Snippet } from 'svelte';
  import { IconButton } from '@kenn-io/kit-ui';
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';
  let { name, email = '', domain = false, onBack, onOpenDetails, actions }:
    { name: string; email?: string; domain?: boolean; onBack?: () => void; onOpenDetails: () => void; actions?: Snippet } = $props();
</script>

<header>
  {#if onBack}<IconButton ariaLabel="Back to People" onclick={onBack}><ArrowLeft size={16} aria-hidden="true" /></IconButton>{/if}
  <button class="avatar" type="button" tabindex="-1" aria-label="Open details" onclick={onOpenDetails}><IdentityAvatar label={name} seed={email || name} shape={domain ? 'domain' : 'person'} size={40} /></button>
  <div><h2><button type="button" onclick={onOpenDetails}>{name}</button></h2>{#if email && email !== name}<span>{email}</span>{/if}</div>
  <div class="actions">{@render actions?.()}</div>
</header>

<style>
  /* Padded 12/16. The 40 avatar, then 12, then the name (Title) over the email (Meta), centered as a block on
   * the avatar; Merge with… and ⋯ are 28, 8 apart, centered on it too. */
  header { display: flex; flex: none; gap: var(--space-box); align-items: center; padding: var(--space-box) var(--space-pane); border-bottom: 1px solid var(--border-muted); }
  header > div { display: flex; min-width: 0; flex-direction: column; gap: 0; }
  h2 { margin: 0; font-size: var(--type-title-size); font-weight: 600; line-height: var(--type-title-line); }
  button { padding: 0; border: 0; background: none; color: inherit; font: inherit; text-align: left; cursor: pointer; }
  h2 button:hover { text-decoration: underline; }
  .avatar { display: flex; flex: none; }
  .actions { flex-direction: row; align-items: center; margin-left: auto; display: flex; flex-wrap: wrap; gap: var(--space-group); }
  span { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); overflow-wrap: anywhere; }
  /* On a phone the name keeps the row, and Merge with… and ⋯ wrap together as one group under it, 8 below, at
   * the header's left inset. */
  @media (max-width: 640px) {
    header { flex-wrap: wrap; row-gap: var(--space-group); }
    header > div { flex: 1 1 0; }
    .actions { flex-basis: 100%; flex-wrap: nowrap; margin-left: 0; }
  }
</style>
