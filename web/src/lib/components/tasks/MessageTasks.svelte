<script lang="ts">
  import type { APIClient } from '../../api/client';
  import { findKataIssues } from '../../api/generated/api/api';
  import type { KataIssueReceipt } from '../../api/generated/models';
  import { kataReadiness } from '../../kata/kata-ready.svelte';
  import TaskLinks from './TaskLinks.svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';

  let { client, messageId, title, email = false, refresh = 0, showSetup = true, onsettings }: {
    client: APIClient; messageId: number; title: string; email?: boolean; refresh?: number; showSetup?: boolean; onsettings?: () => void;
  } = $props();
  const kata = kataReadiness();
  let issues = $state<KataIssueReceipt[]>([]);
  let loading = $state(false);
  let error = $state('');
  let truncated = $state(false);
  let tasksPresent = $state(false);
  $effect(() => {
    const id = messageId;
    void refresh;
    if (!kata?.ready) return;
    const request = new AbortController();
    issues = []; loading = true; error = ''; truncated = false;
    void findKataIssues({ message_id: id }, { ...client, signal: request.signal }).then(({ data, error: failure }) => {
      if (request.signal.aborted) return;
      if (!data) throw new Error(failure?.message || 'Linked Kata issues are unavailable.');
      issues = data.issues;
      truncated = data.truncated;
    }).catch((cause) => {
      if (!request.signal.aborted) error = cause instanceof Error ? cause.message : 'Linked Kata issues are unavailable.';
    }).finally(() => { if (!request.signal.aborted) loading = false; });
    return () => request.abort();
  });
</script>

<div class:task-group={tasksPresent || issues.length > 0} class:task-notices={!tasksPresent && issues.length === 0} role={tasksPresent || issues.length > 0 ? 'region' : undefined} aria-label={tasksPresent || issues.length > 0 ? 'Tasks' : undefined}>
  {#if tasksPresent || issues.length > 0}<h2>Tasks</h2>{/if}
  {#if email}
    {#key refresh}<TaskLinks {client} {messageId} {title} {onsettings} {showSetup} mode="list" embedded bind:present={tasksPresent} />{/key}
  {/if}
  {#if kata?.ready}
    <div class="kata-links">
      {#if loading}<p role="status">Loading linked Kata issues…</p>{/if}
      {#if error}<InlineNotice message={error} />{/if}
      {#if truncated}<InlineNotice tone="warning" message="Linked Kata issues are incomplete." />{/if}
      {#each issues as issue (issue.uid)}
        <DetailRow description={`Kata · ${issue.qualified_ref} · ${issue.status.replaceAll('_', ' ').replace(/^./, value => value.toUpperCase())}`}>
          {issue.title}
          {#snippet actions()}{#if issue.web_url}<a class="text-action" href={issue.web_url} target="_blank" rel="noreferrer">Open in Kata</a>{/if}{/snippet}
        </DetailRow>
      {/each}
    </div>
  {/if}
</div>

<style>
  .task-group { display: grid; gap: var(--space-2); padding: 0 var(--pane-pad-x) var(--space-4); }
  .task-notices { padding: 0 var(--pane-pad-x); }
  .kata-links { display: grid; gap: var(--space-2); }
  .text-action { box-sizing: border-box; display: inline-flex; align-items: center; justify-content: center; min-height: var(--kit-control-height, 28px); padding: 3px 10px; border: var(--border-width) solid var(--border-default); border-radius: var(--kit-control-radius, var(--radius-sm)); background: var(--bg-raised, var(--bg-inset)); color: var(--text-secondary); font-size: var(--kit-control-font-size, var(--font-size-md)); font-weight: var(--font-weight-medium, 500); text-decoration: none; box-shadow: var(--shadow-raised, none); }
  .task-group :global(.task-links), .task-notices :global(.task-links) { padding: 0; }
  @media (max-width: 640px) {
    .task-group :global(.detail-row .actions) { margin-left: 0; }
  }
  h2, p { margin: 0; font-size: var(--font-size-sm); }
</style>
