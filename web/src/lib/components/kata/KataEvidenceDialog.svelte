<script lang="ts">
  import { appShortcuts, Button, Modal, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';
  import { createKataIssue, linkKataEvidence, prepareKataEvidence } from '../../api/generated/api/api';
  import type { APIClient } from '../../api/client';
  import type { KataIssueCreateRequest, KataIssueReceipt } from '../../api/generated/models';
  import { errorMessage } from '../../api/session.svelte';
  import { EVIDENCE_STEP, EVIDENCE_WINDOW, retryKey, type EvidenceSelector, type PreparedEvidence } from '../../kata/evidence';

  let { client, selector, defaultTitle = '', onclose = () => undefined }: {
    client: APIClient;
    selector: EvidenceSelector;
    defaultTitle?: string;
    onclose?: () => void;
  } = $props();

  let mode = $state<'create' | 'link'>('create');
  // The dialog is mounted per source, so props seed its editable state once.
  let title = $state(untrack(() => defaultTitle));
  let brief = $state('');
  let issueRef = $state('');
  let start = $state(untrack(() => selector.start_rune ?? 0));
  // A message citation narrowed to text selected in the preview, in runes.
  let narrowed = $state<{ start: number; end: number }>();
  // Whether the window, not a narrowed selection, has text after it.
  let windowHasMore = $state(false);
  let excerptElement = $state<HTMLPreElement>();
  let preview = $state<PreparedEvidence>();
  let loading = $state(false);
  let busy = $state(false);
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
  const canSubmit = $derived(!busy && !loading && !!preview && (mode === 'create' ? !!title.trim() : !!issueRef.trim()));
  const actionOptions = [{ value: 'create', label: 'Create issue' }, { value: 'link', label: 'Add to existing issue' }];

  $effect(() => { void prepare(start, narrowed); });
  onMount(() => appShortcuts.pushScope('kata-evidence-dialog'));
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
        const { data, error: failure } = await linkKataEvidence({ ref: issueRef.trim() }, { evidence: [preview.reference] }, client);
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

<Modal title="Kata issue" onclose={() => { if (!busy) onclose(); }}>
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
        <label>Action<SelectDropdown title="Action" options={actionOptions} value={mode} disabled={busy} onchange={(value) => mode = value === 'link' ? 'link' : 'create'} /></label>
        {#if mode === 'create'}
          <label>Issue title<TextInput ariaLabel="Issue title" bind:value={title} disabled={busy} autocomplete="off" block /></label>
          <label>Brief<textarea aria-label="Brief" bind:value={brief} disabled={busy} rows="3" maxlength="2000"></textarea></label>
        {:else}
          <label>Kata issue<TextInput ariaLabel="Kata issue ref" bind:value={issueRef} disabled={busy} placeholder="project#abcd" block /></label>
        {/if}
        <section aria-label="Quoted passage">
          <p class="disclosure">This exact text is saved in Kata.</p>
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
        <Button type="submit" tone="info" surface="solid" label={mode === 'create' ? 'Create issue' : 'Add evidence'} disabled={!canSubmit} />
      </form>
    {/if}
    <div class="actions"><Button label="Close" surface="soft" disabled={busy} onclick={onclose} /></div>
  </div>
</Modal>

<style>
  .kata-dialog, form { display: grid; gap: var(--space-3); min-width: 0; }
  .kata-dialog { width: min(28rem, 80vw); }
  p { margin: 0; }
  .disclosure { color: var(--text-secondary); font-size: var(--font-size-sm); }
  label { display: grid; gap: var(--space-1); }
  textarea { resize: vertical; padding: var(--space-2); color: var(--text-primary); background: var(--bg-canvas); border: 1px solid var(--border-default); border-radius: var(--radius-sm); }
  pre { white-space: pre-wrap; overflow-wrap: anywhere; max-height: 14rem; overflow: auto; font: inherit; padding: var(--space-2); border: 1px solid var(--border-muted); border-radius: var(--radius-sm); }
  .passage-controls, .actions { display: flex; gap: var(--space-2); justify-content: flex-end; }
</style>
