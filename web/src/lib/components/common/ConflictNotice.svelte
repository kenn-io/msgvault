<script lang="ts">
  import type { Snippet } from 'svelte';
  import { Disclosure } from '@kenn-io/kit-ui';
  import ChangeList, { type Change } from './ChangeList.svelte';
  import InlineNotice from './InlineNotice.svelte';
  import { conflictSaved } from '../../directory/labels';

  let { noun, rows, other = [], listed = [], saved, more, message, actionLabel, onaction }: {
    noun: string;
    rows: Change[];
    other?: Change[];
    /** Other changes whose saved values `more` already lists; they count as other changes but get no row of their own. */
    listed?: string[];
    saved: { updated_at: string; source?: string };
    more?: Snippet;
    message: string;
    actionLabel?: string;
    onaction?: () => void;
  } = $props();
</script>

<InlineNotice message={`This ${noun} changed since you opened it.`} {actionLabel} {onaction}>
  <ChangeList changes={rows} otherChanged={other.length > 0} provenance={conflictSaved(saved.updated_at, saved.source)} />
  <Disclosure variant="plain" class="fold" title="Details">
    {@const unlisted = other.filter(change => !listed.includes(change.label))}
    {#if unlisted.length}<dl>{#each unlisted as change (change.label)}<div><dt>{change.label}</dt><dd>{change.saved || 'None'}</dd></div>{/each}</dl>{/if}
    {@render more?.()}
    {#if message}<pre>{message}</pre>{/if}
  </Disclosure>
</InlineNotice>

<style>
  dl { display: grid; margin: 0; }
  dl div { display: grid; grid-template-columns: var(--detail-label-width) minmax(0, 1fr); align-items: baseline; gap: var(--space-group); padding-block: var(--row-pad-y); }
  dt { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  dd { margin: 0; overflow-wrap: anywhere; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  pre { margin: 0; max-height: 16rem; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; font-size: var(--type-small-size); line-height: var(--type-small-line); }
</style>
