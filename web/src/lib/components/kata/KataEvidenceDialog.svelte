<script lang="ts">
  import { appShortcuts, Button, escapeCloses, Modal, SegmentedControl, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';
  import { createKataIssue, linkKataEvidence, prepareKataEvidence, listOpenKataIssues } from '../../api/generated/api/api';
  import ExistingPicker from '../common/ExistingPicker.svelte';
  import type { APIClient } from '../../api/client';
  import type { KataIssueCreateRequest, KataIssueReceipt } from '../../api/generated/models';
  import { errorMessage } from '../../api/session.svelte';
  import { EVIDENCE_STEP, EVIDENCE_WINDOW, retryKey, type EvidenceSelector, type PreparedEvidence } from '../../kata/evidence';

  let { client, selector, defaultTitle = '', onclose = () => undefined, oncreated = () => undefined, embedded = false, busy = $bindable(false) }: {
    client: APIClient;
    embedded?: boolean;
    busy?: boolean;
    selector: EvidenceSelector;
    defaultTitle?: string;
    onclose?: () => void;
    oncreated?: () => void;
  } = $props();

  let mode = $state<'create' | 'link'>('create');
  // The dialog is mounted per source, so props seed its editable state once.
  let title = $state(untrack(() => defaultTitle));
  let brief = $state('');
  let selected = $state('');
  let start = $state(untrack(() => selector.start_rune ?? 0));
  // A message citation narrowed to text selected in the preview, in runes.
  let narrowed = $state<{ start: number; end: number }>();
  // Whether the window, not a narrowed selection, has text after it.
  let windowHasMore = $state(false);
  let excerptElement = $state<HTMLPreElement>();
  let preview = $state<PreparedEvidence>();
  let loading = $state(false);
  let error = $state('');
  let receipt = $state<KataIssueReceipt>();
  let replayed = $state(false);
  // The key whose issue was deleted in Kata; filing again salts from it.
  let deletedKey = $state('');
  // Salting with the replayed issue gives a second issue a key that survives a remount.
  let salt = '';
  let generation = 0;
  let controller: AbortController | undefined;
  // Exact ranges come from a chosen file passage; windows page through a body.
  const windowed = $derived(selector.end_rune == null);
  const canSubmit = $derived(!busy && !loading && !!preview && (mode === 'create' ? !!title.trim() : !!selected));
  const actionOptions = [{ value: 'create', label: 'New' }, { value: 'link', label: 'Existing' }];

  let openIssues: ReturnType<typeof listOpenKataIssues> | undefined;
  async function loadIssues(query: string, _signal: AbortSignal) {
    openIssues ??= listOpenKataIssues(client);
    let data;
    try {
      const response = await openIssues;
      if (!response.data) throw new Error(errorMessage(response.error, 'Unable to load issues.'));
      data = response.data;
    } catch (cause) {
      openIssues = undefined;
      throw cause;
    }
    const text = query.trim();
    const filter = text.toLowerCase();
    const items = data.issues.filter(issue => issue.title.toLowerCase().includes(filter) || issue.qualified_ref.toLowerCase().includes(filter))
      .map(issue => ({ id: issue.qualified_ref, label: issue.title, detail: issue.qualified_ref }));
    if (text && items.length === 0) items.push({ id: text, label: `Add quote to ${text}`, detail: '' });
    return { items, truncated: data.truncated };
  }

  $effect(() => { void prepare(start, narrowed); });
  $effect(() => { if (receipt) untrack(oncreated); });
  onMount(() => { if (!embedded) return appShortcuts.pushScope('kata-evidence-dialog'); });
  onDestroy(() => { generation++; controller?.abort(); });

  async function prepare(from: number, exact: { start: number; end: number } | undefined): Promise<void> {
    const current = ++generation;
    controller?.abort();
    const signal = (controller = new AbortController()).signal;
    loading = true; error = ''; preview = undefined;
    const { max_chars: _window, ...base } = selector;
    let request: EvidenceSelector = selector;
    if (exact) request = { ...base, start_rune: exact.start, end_rune: exact.end };
    else if (windowed) request = { ...selector, start_rune: from, max_chars: EVIDENCE_WINDOW };
    try {
      const { data, error: failure } = await prepareKataEvidence({ selectors: [request] }, { ...client, signal });
      if (current !== generation) return;
      if (!data?.evidence[0]) throw new Error(errorMessage(failure, 'This passage is unavailable.'));
      preview = data.evidence[0];
      if (!exact) windowHasMore = !!preview.next_rune;
    } catch (cause) {
      if (current === generation && !signal.aborted) error = errorMessage(cause, 'This passage is unavailable.');
    } finally {
      if (current === generation) loading = false;
    }
  }

  // Offsets count code points, matching the server's runes, so text such as
  // emoji before the selection does not shift the cited range.
  function quoteSelection(): void {
    const selection = window.getSelection();
    const range = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : undefined;
    const origin = (preview?.reference.message ?? preview?.reference.document_chunk)?.start_rune;
    if (!range || range.collapsed || !excerptElement || origin == null ||
      !excerptElement.contains(range.startContainer) || !excerptElement.contains(range.endContainer)) {
      error = 'Select part of the passage first.';
      return;
    }
    const before = document.createRange();
    before.selectNodeContents(excerptElement);
    before.setEnd(range.startContainer, range.startOffset);
    const offset = [...before.toString()].length;
    narrowed = { start: origin + offset, end: origin + offset + [...range.toString()].length };
  }

  function page(to: number): void {
    narrowed = undefined;
    start = to;
  }

  async function submit(): Promise<void> {
    if (!canSubmit || !preview) return;
    busy = true; error = ''; deletedKey = '';
    try {
      if (mode === 'create') {
        const request: KataIssueCreateRequest = { title: title.trim(), evidence: [preview.reference] };
        if (brief.trim()) request.brief = brief.trim();
        const key = retryKey(request, [preview], salt);
        const { data, error: failure } = await createKataIssue(request, { ...client, headers: { 'Idempotency-Key': key } });
        if (failure?.error === 'kata_issue_deleted') {
          deletedKey = key;
          throw new Error(errorMessage(failure, 'The issue this key filed was deleted in Kata or is not visible to this credential.'));
        }
        if (!data) throw new Error(errorMessage(failure, 'The issue may not have been created. Submit again to retry safely.'));
        receipt = data.issue;
        replayed = data.replayed;
      } else {
        const { data, error: failure } = await linkKataEvidence({ ref: selected }, { evidence: [preview.reference] }, client);
        if (!data) throw new Error(errorMessage(failure, 'The evidence could not be added.'));
        receipt = data.issue;
        replayed = false;
      }
    } catch (cause) {
      error = errorMessage(cause, 'Kata is unavailable.');
    } finally {
      busy = false;
    }
  }

  async function fileAgain(): Promise<void> {
    if (!receipt && !deletedKey) return;
    salt = receipt ? `after:${receipt.uid}` : `after-deleted:${deletedKey}`;
    receipt = undefined;
    replayed = false;
    await submit();
  }
</script>

<svelte:window onkeydowncapture={(event) => { if (!embedded) escapeCloses(() => { if (!busy) onclose(); })(event); }} />

{#if embedded}
  {@render body()}
{:else}
  <Modal width="520px" maxWidth="calc(100vw - 32px)" title="Kata issue" onclose={() => { if (!busy) onclose(); }}>
    {@render body()}
  </Modal>
{/if}

{#snippet body()}
  <div class="kata-dialog">
    {#if receipt}
      {#if replayed}
        <p role="status">Already filed as <strong>{receipt.qualified_ref}</strong> ({receipt.status}).</p>
      {:else}
        <p role="status">Saved to <strong>{receipt.qualified_ref}</strong>.</p>
      {/if}
      {#if receipt.web_url}<a href={receipt.web_url} target="_blank" rel="noreferrer">Open in Kata</a>{/if}
      {#if replayed}<Button label="File as a new issue" disabled={busy} onclick={() => void fileAgain()} />{/if}
      {#if error}<p role="alert">{error}</p>{/if}
    {:else}
      <form onsubmit={(event) => { event.preventDefault(); void submit(); }} aria-busy={busy}>
        <SegmentedControl ariaLabel="Issue" options={actionOptions} value={mode} disabled={busy} onchange={(value) => mode = value === 'link' ? 'link' : 'create'} />
        {#if mode === 'create'}
          <label>Issue title<TextInput ariaLabel="Issue title" bind:value={title} disabled={busy} autocomplete="off" block /></label>
          <label>Brief<textarea aria-label="Brief" bind:value={brief} disabled={busy} rows="3" maxlength="2000"></textarea></label>
        {:else}
          <ExistingPicker load={loadIssues} noun="open issues" moreNote="Showing the 25 newest open issues. Type an issue's ref to add it." empty="No open issues yet." disabled={busy} bind:selected />
        {/if}
        <section aria-label="Quoted passage">
          <p class="disclosure">Quote saved in Kata</p>
          {#if loading}<p role="status">Preparing the passage…</p>{/if}
          {#if preview}<pre bind:this={excerptElement}>{preview.excerpt}</pre>{/if}
          {#if windowed}
            <div class="passage-controls">
              {#if narrowed}
                <Button size="sm" surface="soft" label="Whole passage" disabled={busy || loading} onclick={() => page(start)} />
              {:else}
                <Button size="sm" surface="soft" label="Quote selection" disabled={busy || loading || !preview} onclick={quoteSelection} />
              {/if}
              <Button size="sm" surface="soft" label="Earlier passage" disabled={busy || loading || start === 0} onclick={() => page(Math.max(0, start - EVIDENCE_STEP))} />
              <Button size="sm" surface="soft" label="Later passage" disabled={busy || loading || !windowHasMore} onclick={() => page(start + EVIDENCE_STEP)} />
            </div>
          {/if}
        </section>
        {#if error}<p role="alert">{error}</p>{/if}
        {#if deletedKey}<Button label="File as a new issue" disabled={busy} onclick={() => void fileAgain()} />{/if}
        <div class="actions"><Button label="Cancel" surface="soft" disabled={busy} onclick={onclose} />
        <Button type="submit" tone="info" surface="solid" label={mode === 'create' ? 'Create issue' : 'Add quote'} disabled={!canSubmit} /></div>
      </form>
    {/if}
    {#if receipt}<div class="actions"><Button label="Done" tone="info" surface="solid" disabled={busy} onclick={onclose} /></div>{/if}
  </div>
{/snippet}

<style>
  .kata-dialog, form { display: grid; gap: var(--space-6); min-width: 0; }
  .kata-dialog { width: 100%; }
  p { margin: 0; }
  .disclosure { color: var(--text-secondary); font-size: var(--font-size-sm); }
  label { display: grid; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-size-sm); }
  label :global(.kit-text-input__control), textarea { font-size: 16px; }
  textarea { resize: vertical; padding: var(--space-2); color: var(--text-primary); background: var(--bg-canvas); border: 1px solid var(--border-default); border-radius: var(--radius-sm); }
  section { display: grid; gap: var(--space-2); }
  pre { margin: 0; white-space: pre-wrap; overflow-wrap: anywhere; max-height: 14rem; overflow: auto; font: inherit; padding: var(--space-2); border: 1px solid var(--border-muted); border-radius: var(--radius-sm); }
  .passage-controls { display: flex; gap: var(--space-2); justify-content: flex-end; margin-top: var(--space-2); }
  .actions { display: flex; gap: var(--space-4); justify-content: flex-end; }
</style>
