<script lang="ts">
  import { onDestroy } from 'svelte';
  import { Button, escapeCloses, TextInput } from '@kenn-io/kit-ui';
  import { searchDocuments } from '../../api/generated/api/api';
  import type { APIClient } from '../../api/client';
  import type { DocumentSearchResult } from '../../api/generated/models';
  import { errorMessage } from '../../api/session.svelte';
  import { EVIDENCE_WINDOW, type EvidenceSelector } from '../../kata/evidence';
  import { kataReadiness } from '../../kata/kata-ready.svelte';
  import { reportError, reportSearch, reportWhenVisible } from '../../telemetry/actions';
  import Dialog from '../common/Dialog.svelte';
  import KataEvidenceDialog from './KataEvidenceDialog.svelte';

  let { client, attachmentID, messageID, filename }: {
    client: APIClient; attachmentID: number; messageID: number; filename: string;
  } = $props();
  const kata = kataReadiness();
  let open = $state(false);
  let query = $state('');
  let hits = $state<DocumentSearchResult[]>([]);
  let selected = $state<EvidenceSelector>();
  let busy = $state(false);
  let searched = $state(false);
  let error = $state('');
  let generation = 0;
  let controller: AbortController | undefined;
  onDestroy(close);

  async function search(): Promise<void> {
    if (!query.trim() || busy) return;
    const request = ++generation, submitted = query.trim();
    controller?.abort();
    const requestController = new AbortController();
    controller = requestController;
    busy = true; error = ''; hits = []; searched = true;
    try {
      const { data, error: failure } = await searchDocuments({ q: submitted, attachment_id: attachmentID, message_id: messageID, mode: 'lexical', limit: 20 }, { ...client, signal: requestController.signal });
      if (request !== generation) return;
      if (!data) throw new Error(errorMessage(failure, 'This file has no text to search.'));
      // A filename match names the file, not a passage in it.
      hits = data.results.filter((hit) => hit.attachment_id === attachmentID && hit.message_id === messageID && hit.extraction_id && hit.chunk_key && hit.matched_signals.includes('content'));
      reportWhenVisible(() => reportSearch(client, submitted, 'full_text', hits.length), requestController.signal);
    } catch (cause) {
      if (request === generation) { error = errorMessage(cause, 'Unable to search this file.'); reportError(() => client, () => 'search', requestController.signal); }
    } finally {
      if (request === generation) busy = false;
    }
  }

  // A search excerpt can sit anywhere in its chunk, so the window opens at the
  // excerpt's own offset; the dialog pages and narrows it like a message.
  function choose(hit: DocumentSearchResult): void {
    selected = { kind: 'document_chunk', attachment_id: attachmentID, message_id: messageID, extraction_id: hit.extraction_id, chunk_key: hit.chunk_key, start_rune: hit.excerpt_start_rune, max_chars: EVIDENCE_WINDOW };
  }

  function close(): void { generation++; controller?.abort(); busy = false; open = false; selected = undefined; }

  // Every kit-ui Modal hears Escape on the window, and the file viewer's was
  // registered first; claiming it during capture closes only this picker.
  const pickerEscape = escapeCloses(close);
</script>

<svelte:window onkeydowncapture={(event) => { if (open && !selected) pickerEscape(event); }} />

{#if kata?.ready}<Button label="Add to task…" size="sm" surface="soft" onclick={() => open = true} />{/if}
{#if open && !selected}
  <Dialog title="Add to task" onclose={close}>
    <div class="passage-picker">
      <form onsubmit={(event) => { event.preventDefault(); void search(); }}>
        <label>Quote from {filename}<span class="search-row"><TextInput ariaLabel="Search this file" placeholder="Search this file" bind:value={query} disabled={busy} block /><Button type="submit" label="Search" disabled={busy || !query.trim()} /></span></label>
      </form>
      {#if busy}<p role="status">Searching…</p>{/if}
      {#if error}<p role="alert">{error}</p>{/if}
      {#each hits as hit, index (`${hit.extraction_id}:${hit.chunk_key}:${hit.excerpt_start_rune}`)}
        <article><pre>{hit.excerpt}</pre><Button label="Use this quote" ariaLabel={`Use quote ${index + 1}`} onclick={() => choose(hit)} /></article>
      {/each}
      {#if searched && !busy && !error && !hits.length}<p>No matches.</p>{/if}
    </div>
  </Dialog>
{/if}
{#if open && selected}
  <KataEvidenceDialog {client} selector={selected} defaultTitle={`Follow up on ${filename}`} onclose={close} />
{/if}

<style>
  .passage-picker, form, article { display: grid; gap: var(--space-3); }
  pre { margin: 0; white-space: pre-wrap; overflow-wrap: anywhere; font: inherit; font-size: var(--font-size-sm); }
  p { margin: 0; color: var(--text-muted); font-size: var(--font-size-sm); }
  label { display: grid; gap: var(--space-2); color: var(--text-muted); font-size: var(--font-size-xs); }
  .search-row { display: flex; align-items: center; gap: var(--space-3); }
</style>
