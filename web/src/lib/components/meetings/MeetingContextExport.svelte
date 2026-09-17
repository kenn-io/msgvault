<script lang="ts">
  import { Button, Checkbox, SegmentedControl, type SegmentedControlOption } from '@kenn-io/kit-ui';
  import { onDestroy, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type {
    MeetingContextRequest,
    MeetingContextRequestFormat,
    PacketResult,
  } from '../../api/generated/models';
  import { createMeetingsAPI, MeetingsAPIError } from '../../meetings/api';

  let {
    client,
    request,
    disabledReason = '',
  }: {
    client: APIClient;
    request: MeetingContextRequest;
    disabledReason?: string;
  } = $props();

  const formatOptions: SegmentedControlOption[] = [
    { value: 'json', label: 'JSON' },
    { value: 'markdown', label: 'Markdown' },
  ];
  const api = createMeetingsAPI(untrack(() => client));
  let format = $state<MeetingContextRequestFormat>('json');
  let includeTranscript = $state(false);
  let exporting = $state(false);
  let error = $state('');
  let result = $state<PacketResult>();
  let requestController: AbortController | undefined;
  let requestGeneration = 0;
  const requestFingerprint = $derived(JSON.stringify(request));

  $effect(() => {
    void requestFingerprint;
    requestGeneration += 1;
    requestController?.abort();
    requestController = undefined;
    exporting = false;
    error = '';
    result = undefined;
  });

  onDestroy(() => {
    requestGeneration += 1;
    requestController?.abort();
  });

  function errorMessage(cause: unknown): string {
    if (cause instanceof MeetingsAPIError) {
      if (cause.code === 'selection_not_all_meetings') return 'Select meetings only';
      if (cause.status === 404 && cause.code === 'not_found') {
        return 'Meeting context export is unavailable with this daemon.';
      }
      return cause.message;
    }
    return cause instanceof Error ? cause.message : 'Meeting context could not be exported.';
  }

  async function exportContext(): Promise<void> {
    if (disabledReason || exporting) return;
    requestController?.abort();
    const controller = new AbortController();
    requestController = controller;
    const generation = ++requestGeneration;
    exporting = true;
    error = '';
    result = undefined;
    const submittedFormat = format;
    const submitted = {
      ...request,
      format: submittedFormat,
      include_transcript: includeTranscript,
    } as MeetingContextRequest;
    try {
      const exported = await api.context(submitted, controller.signal);
      if (generation !== requestGeneration || controller.signal.aborted) return;
      result = exported;
      await tick();
      if (generation !== requestGeneration || controller.signal.aborted) return;
      const mimeType = submittedFormat === 'json' ? 'application/json' : 'text/markdown';
      const extension = submittedFormat === 'json' ? 'json' : 'md';
      const objectURL = URL.createObjectURL(new Blob([exported.content], { type: mimeType }));
      try {
        const anchor = document.createElement('a');
        anchor.href = objectURL;
        anchor.download = `meeting-context.${extension}`;
        anchor.click();
      } finally {
        URL.revokeObjectURL(objectURL);
      }
    } catch (cause) {
      if (generation !== requestGeneration || controller.signal.aborted) return;
      error = errorMessage(cause);
    } finally {
      if (generation === requestGeneration) {
        exporting = false;
        requestController = undefined;
      }
    }
  }
</script>

<section class="meeting-export" aria-label="Meeting context export">
  <SegmentedControl
    ariaLabel="Meeting context format"
    options={formatOptions}
    value={format}
    onchange={(value) => (format = value as MeetingContextRequestFormat)}
  />
  <Checkbox
    checked={includeTranscript}
    label="Include transcript"
    disabled={exporting || Boolean(disabledReason)}
    onchange={(checked) => (includeTranscript = checked)}
  />
  <Button
    size="sm"
    tone="info"
    surface="soft"
    label={exporting ? 'Exporting meeting context…' : 'Export meeting context'}
    ariaLabel="Export meeting context"
    disabled={exporting || Boolean(disabledReason)}
    onclick={() => void exportContext()}
  />
  {#if disabledReason}
    <span class="meeting-export__notice" role="status">{disabledReason}</span>
  {/if}
  {#if error}
    <span class="meeting-export__error" role="alert">{error}</span>
  {/if}
  {#if result?.truncated}
    <span class="meeting-export__notice" role="status">
      Export was truncated.
      {#if result.omitted_message_ids.length > 0}
        Omitted meeting IDs: {result.omitted_message_ids.join(', ')}.
      {/if}
    </span>
  {/if}
</section>

<style>
  .meeting-export {
    display: flex;
    min-width: 0;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .meeting-export__notice,
  .meeting-export__error {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    overflow-wrap: anywhere;
  }

  .meeting-export__error {
    color: var(--text-danger);
  }
</style>
