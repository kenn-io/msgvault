<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';
  let { name, email = '', domain = false, onBack, onOpenDetails }:
    { name: string; email?: string; domain?: boolean; onBack?: () => void; onOpenDetails: () => void } = $props();
</script>

<header>
  {#if onBack}<Button label="Back to People" onclick={onBack} />{/if}
  <IdentityAvatar label={name} seed={email || name} shape={domain ? 'domain' : 'person'} size={40} />
  <div><h2><button type="button" onclick={onOpenDetails}>{name}</button></h2>{#if email}<span>{email}</span>{/if}</div>
</header>

<style>
  header { display: flex; gap: var(--control-gap); align-items: flex-start; padding: var(--pane-pad-y) var(--page-gutter); border-bottom: 1px solid var(--border-muted); }
  header > div { display: flex; min-width: 0; flex-direction: column; gap: var(--space-1); }
  h2 { margin: 0; font-size: var(--font-size-lg); }
  button { padding: 0; border: 0; background: none; color: inherit; font: inherit; text-align: left; cursor: pointer; }
  button:hover { text-decoration: underline; }
  span { color: var(--text-muted); font-size: var(--font-size-sm); overflow-wrap: anywhere; }
</style>
