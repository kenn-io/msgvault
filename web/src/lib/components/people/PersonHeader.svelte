<script lang="ts">
  import type { Snippet } from 'svelte';
  import { Button } from '@kenn-io/kit-ui';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';
  let { name, email = '', domain = false, onBack, onOpenDetails, actions }:
    { name: string; email?: string; domain?: boolean; onBack?: () => void; onOpenDetails: () => void; actions?: Snippet } = $props();
</script>

<header>
  {#if onBack}<Button label="Back to People" onclick={onBack} />{/if}
  <button class="avatar" type="button" aria-label="Open details" onclick={onOpenDetails}><IdentityAvatar label={name} seed={email || name} shape={domain ? 'domain' : 'person'} size={40} /></button>
  <div><h2><button type="button" onclick={onOpenDetails}>{name}</button></h2>{#if email}<span>{email}</span>{/if}</div>
  <div class="actions">{@render actions?.()}</div>
</header>

<style>
  header { display: flex; gap: var(--control-gap); align-items: flex-start; padding: var(--pane-pad-y) var(--page-gutter); border-bottom: 1px solid var(--border-muted); }
  header > div { display: flex; min-width: 0; flex-direction: column; gap: var(--space-1); }
  h2 { margin: 0; font-size: var(--font-size-lg); }
  button { padding: 0; border: 0; background: none; color: inherit; font: inherit; text-align: left; cursor: pointer; }
  button:hover { text-decoration: underline; }
  .actions { margin-left: auto; display: flex; flex-wrap: wrap; gap: var(--control-gap); }
  span { color: var(--text-muted); font-size: var(--font-size-sm); overflow-wrap: anywhere; }
</style>
