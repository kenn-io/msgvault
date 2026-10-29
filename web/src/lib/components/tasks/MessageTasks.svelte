<script lang="ts">
  import { onDestroy } from 'svelte';
  import type { APIClient } from '../../api/client';
  import { findKataIssues } from '../../api/generated/api/api';
  import type { KataIssueReceipt } from '../../api/generated/models';
  import { kataReadiness } from '../../kata/kata-ready.svelte';
  import TaskLinks from './TaskLinks.svelte';

  let { client, messageId, title, email = false, refresh = 0, onsettings }: {
    client: APIClient; messageId: number; title: string; email?: boolean; refresh?: number; onsettings?: () => void;
  } = $props();
  const kata = kataReadiness();
  let issues = $state<KataIssueReceipt[]>([]);
  let loading = $state(false);
  let error = $state('');
  let controller: AbortController | undefined;
  $effect(() => {
    const id = messageId;
    void refresh;
    if (!kata?.ready) return;
    controller?.abort();
    const request = controller = new AbortController();
    issues = []; loading = true; error = '';
    void findKataIssues({ message_id: id }, { ...client, signal: request.signal }).then(({ data, error: failure }) => {
      if (request.signal.aborted) return;
      if (!data) throw new Error(failure?.message || 'Linked Kata issues are unavailable.');
      issues = data.issues;
    }).catch((cause) => {
      if (!request.signal.aborted) error = cause instanceof Error ? cause.message : 'Linked Kata issues are unavailable.';
    }).finally(() => { if (!request.signal.aborted) loading = false; });
    return () => request.abort();
  });
  onDestroy(() => controller?.abort());
</script>

{#if email}
  {#key refresh}<TaskLinks {client} {messageId} {title} {onsettings} mode="list" />{/key}
{/if}
{#if kata?.ready}
  <section class="kata-links" aria-label="Linked Kata issues">
    <h2>Kata issues</h2>
    {#if loading}<p role="status">Loading linked Kata issues…</p>{/if}
    {#if error}<p role="alert">{error}</p>{/if}
    {#each issues as issue (issue.uid)}
      <p>{#if issue.web_url}<a href={issue.web_url} target="_blank" rel="noreferrer">{issue.qualified_ref}: {issue.title}</a>{:else}{issue.qualified_ref}: {issue.title}{/if} · {issue.status}</p>
    {:else}{#if !loading && !error}<p>No linked Kata issues.</p>{/if}{/each}
  </section>
{/if}

<style>
  .kata-links { display: grid; gap: var(--space-2); padding: 0 var(--pane-pad-x) var(--space-4); }
  h2, p { margin: 0; font-size: var(--font-size-sm); }
</style>
