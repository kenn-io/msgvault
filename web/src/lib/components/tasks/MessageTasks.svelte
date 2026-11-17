<script lang="ts">
  import type { APIClient } from '../../api/client';
  import { findKataIssues } from '../../api/generated/api/api';
  import type { KataIssueReceipt } from '../../api/generated/models';
  import { kataReadiness } from '../../kata/kata-ready.svelte';
  import TaskLinks from './TaskLinks.svelte';

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

<section class="task-group" aria-label="Tasks" hidden={!tasksPresent && !loading && !error && !truncated && issues.length === 0}>
  <h2>Tasks</h2>
  {#if email}
    {#key refresh}<TaskLinks {client} {messageId} {title} {onsettings} {showSetup} mode="list" embedded bind:present={tasksPresent} />{/key}
  {/if}
  {#if kata?.ready}
    <div class="kata-links">
      {#if loading}<p role="status">Loading linked Kata issues…</p>{/if}
      {#if error}<p role="alert">{error}</p>{/if}
      {#if truncated}<p role="status">Linked Kata issues are incomplete.</p>{/if}
      {#each issues as issue (issue.uid)}
        <div class="row"><span>{issue.title}<small>Kata · {issue.qualified_ref}</small></span>{#if issue.web_url}<a href={issue.web_url} target="_blank" rel="noreferrer">Open in Kata</a>{/if}</div>
      {/each}
    </div>
  {/if}
</section>

<style>
  .task-group { display: grid; gap: var(--space-2); padding: 0 var(--pane-pad-x) var(--space-4); }
  .task-group[hidden] { display: none; }
  .kata-links { display: grid; gap: var(--space-2); }
  .row { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); }
  small { display: block; color: var(--text-muted); }
  .task-group :global(.task-links) { padding: 0; }
  h2, p { margin: 0; font-size: var(--font-size-sm); }
</style>
