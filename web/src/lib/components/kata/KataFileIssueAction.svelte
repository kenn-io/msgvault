<script lang="ts">
  import { Button, escapeCloses, Modal, TextInput } from '@kenn-io/kit-ui';
  import { searchDocuments } from '../../api/generated/api/api';
  import type { APIClient } from '../../api/client';
  import type { DocumentSearchResult } from '../../api/generated/models';
  import { errorMessage } from '../../api/session.svelte';
  import { EVIDENCE_WINDOW, type EvidenceSelector } from '../../kata/evidence';
  import { kataReadiness } from '../../kata/kata-ready.svelte';
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


  async function search(): Promise<void> {
    if (!query.trim() || busy) return;
    busy = true; error = ''; hits = []; searched = true;
    try {
      const { data, error: failure } = await searchDocuments({ q: query.trim(), attachment_id: attachmentID, message_id: messageID, mode: 'lexical', limit: 20 }, client);
      if (!data) throw new Error(errorMessage(failure, 'This file has no searchable text yet.'));
      // A filename match names the file, not a passage in it.
      hits = data.results.filter((hit) => hit.attachment_id === attachmentID && hit.message_id === messageID && hit.extraction_id && hit.chunk_key && hit.matched_signals.includes('content'));
    } catch (cause) {
      error = errorMessage(cause, 'Unable to search this file.');
    } finally {
      busy = false;
    }
  }

  // A search excerpt can sit anywhere in its chunk, so the window opens at the
  // excerpt's own offset; the dialog pages and narrows it like a message.
  function choose(hit: DocumentSearchResult): void {
    selected = { kind: 'document_chunk', attachment_id: attachmentID, message_id: messageID, extraction_id: hit.extraction_id, chunk_key: hit.chunk_key, start_rune: hit.excerpt_start_rune, max_chars: EVIDENCE_WINDOW };
  }

  function close(): void { open = false; selected = undefined; }

  // Every kit-ui Modal hears Escape on the window, and the file viewer's was
  // registered first; claiming it during capture closes only this picker.
  const pickerEscape = escapeCloses(close);
</script>

<svelte:window onkeydowncapture={(event) => { if (open && !selected) pickerEscape(event); }} />

{#if kata?.ready}<Button label="Create Kata issue from file" size="sm" surface="soft" onclick={() => open = true} />{/if}
{#if open && !selected}
  <Modal title="Choose a passage" onclose={close}>
    <div class="passage-picker">
      <p>{filename}</p>
      <form onsubmit={(event) => { event.preventDefault(); void search(); }}>
        <label>Find a passage<TextInput ariaLabel="Find a passage" bind:value={query} disabled={busy} block /></label>
        <Button type="submit" label="Search this file" disabled={busy || !query.trim()} />
      </form>
      {#if busy}<p role="status">Searching this file…</p>{/if}
      {#if error}<p role="alert">{error}</p>{/if}
      {#each hits as hit, index (`${hit.extraction_id}:${hit.chunk_key}:${hit.excerpt_start_rune}`)}
        <article><pre>{hit.excerpt}</pre><Button label={`Use passage ${index + 1}`} onclick={() => choose(hit)} /></article>
      {/each}
      {#if searched && !busy && !error && !hits.length}<p>No indexed passage in this file matches.</p>{/if}
    </div>
  </Modal>
{/if}
{#if open && selected}
  <KataEvidenceDialog {client} selector={selected} defaultTitle={`Follow up on ${filename}`} onclose={close} />
{/if}

<style>.passage-picker, form, article { display: grid; gap: var(--space-3); } pre { white-space: pre-wrap; overflow-wrap: anywhere; font: inherit; } label { display: grid; gap: var(--space-1); }</style>
