<script lang="ts">
  import { Button, Checkbox, TextInput } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import type { APIResponse } from '../../api/runtime';
  import { getEmailFamily, previewEmailFamilyDecision, applyEmailFamilyDecision, listEmailFamilyDecisions, getEmailFamilyDecision, undoEmailFamilyDecision, exportEmailFamily } from '../../api/generated/api/api';
  import type { EmailFamily, EmailFamilyMember, EmailFamilyDecision, EmailFamilyDecisionInput, EmailFamilyDecisionPreview, EmailFamilyDecisionResult } from '../../api/generated/models';

  let { client, messageID }: { client: APIClient; messageID: number } = $props();
  let family = $state<EmailFamily>();
  let open = $state(false);
  let linking = $state(false);
  let otherIDs = $state('');
  let selected = $state<number[]>([]);
  let error = $state('');
  let busy = $state(false);
  let pending = $state<{ input: EmailFamilyDecisionInput; preview: EmailFamilyDecisionPreview }>();
  let last = $state<EmailFamilyDecisionResult>();
  let history = $state<EmailFamilyDecision[]>();
  let generation = 0;
  let abort = new AbortController();
  const groups = $derived.by(() => {
    const accounts = new Map<string, EmailFamilyMember[]>();
    for (const member of family?.members ?? []) {
      const key = member.account_address || member.source_identifier || `Source ${member.source_id}`;
      accounts.set(key, [...(accounts.get(key) ?? []), member]);
    }
    return Array.from(accounts);
  });
  const showPanel = $derived(!!family && (family.members.length > 1 || family.truncated || family.members.some(member => member.linkage.outcome !== 'complete')));

  function data<T>(response: APIResponse<T>): T {
    if (!response.response.ok || !response.data) throw Object.assign(new Error(response.error?.message || 'Archive request failed. Try again.'), { code: response.error?.error });
    return response.data;
  }
  const reason = (value: string) => ({
    provider_evidence_unavailable: 'The archive has no trusted Sent evidence for this account.',
    attribution_incomplete: 'Sender or recipient evidence is incomplete.',
    identity_unavailable: 'Sender, recipient or account identity evidence is incomplete.',
    linkage_unavailable: 'Some original identifiers could not be extracted.',
    local_confirmation: 'The selected copies were marked handled locally.',
    chronology_unavailable: 'Dates do not establish which message came later.',
    family_truncated: 'The family exceeds the lookup limit. Handling is unknown.',
    no_received_item: 'No incoming item was identified.',
  }[value] || value.replaceAll('_', ' '));

  $effect(() => {
    const id = messageID;
    const api = client;
    const current = ++generation;
    abort.abort(); abort = new AbortController();
    family = undefined; selected = []; pending = undefined; last = undefined; history = undefined;
    open = false; linking = false; otherIDs = ''; error = ''; busy = false;
    void getEmailFamily({ id }, undefined, { ...api, signal: abort.signal }).then(response => {
      if (current === generation) { const result = data(response); if (!Array.isArray(result.members) || !result.handling) throw new Error('Related copies are unavailable.'); family = result; }
    }).catch(err => { if (current === generation && !abort.signal.aborted) error = err.message; });
    return () => { generation++; abort.abort(); };
  });

  async function action(work: (current: number) => Promise<void>) {
    if (busy) return;
    const current = generation;
    busy = true; error = '';
    try { await work(current); }
    catch (err) {
      if (current === generation) {
        error = err instanceof Error ? err.message : 'Archive request failed.';
        if (err instanceof Error && 'code' in err && typeof err.code === 'string' && ['family_changed', 'family_truncated', 'request_conflict', 'not_found'].includes(err.code)) {
          pending = undefined; last = undefined; history = undefined;
          if (err.code === 'family_changed') error = 'Archive evidence changed. Reload Decision history or preview the selection again.';
        }
      }
    }
    finally { if (current === generation) busy = false; }
  }
  function toggle(id: number) {
    selected = selected.includes(id) ? selected.filter(value => value !== id) : [...selected, id];
    pending = undefined;
  }
  async function preview(kind: 'link' | 'handled') {
    await action(async current => {
      const ids = [...selected];
      if (kind === 'link') {
        ids.push(messageID);
        for (const token of otherIDs.split(/[\s,]+/).filter(Boolean)) {
          const id = Number(token);
          if (!/^\d+$/.test(token) || !Number.isSafeInteger(id) || id < 1) throw new Error('Enter positive archive message IDs separated by spaces or commas.');
          ids.push(id);
        }
      }
      const message_ids = [...new Set(ids)].sort((a, b) => a - b);
      if (message_ids.length < (kind === 'link' ? 2 : 1) || message_ids.length > 100) throw new Error(kind === 'link' ? 'Select or enter 2–100 copies to link.' : 'Select 1–100 exact copies.');
      const bytes = crypto.getRandomValues(new Uint8Array(16));
      const input: EmailFamilyDecisionInput = { kind, message_ids, request_key: Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join(''), actor_kind: 'owner', actor_ref: '', expected_family_revision: '' };
      const result = data(await previewEmailFamilyDecision(input, { ...client, signal: abort.signal }));
      if (current === generation) pending = { input, preview: result };
    });
  }
  async function confirm() {
    if (!pending) return;
    const reviewed = pending;
    await action(async current => {
      const result = data(await applyEmailFamilyDecision({ ...reviewed.input, expected_family_revision: reviewed.preview.before_revision }, { ...client, signal: abort.signal }));
      if (current !== generation) return;
      last = result; family = result.family; pending = undefined; selected = []; history = undefined;
      try {
        const refreshed = data(await getEmailFamily({ id: messageID }, undefined, { ...client, signal: abort.signal }));
        if (current === generation) family = refreshed;
      } catch {
        if (current === generation) error = 'Decision saved, but related copies could not be refreshed. Reload the message to see current archive evidence.';
      }
    });
  }
  async function undo(decision: EmailFamilyDecision) {
    await action(async current => {
      const decisionNow = data(await getEmailFamilyDecision({ id: decision.id }, undefined, { ...client, signal: abort.signal }));
      // Keep the revision the owner saw; a changed decision requires fresh history.
      const result = data(await undoEmailFamilyDecision({ id: decision.id }, { expected_revision: decision.revision, expected_family_revision: decisionNow.family.revision }, { ...client, signal: abort.signal }));
      if (current !== generation) return;
      family = result.family; last = undefined; history = undefined; pending = undefined; selected = [];
      try {
        const refreshed = data(await getEmailFamily({ id: messageID }, undefined, { ...client, signal: abort.signal }));
        if (current === generation) family = refreshed;
      } catch (err) {
        if (current !== generation) return;
        if (err instanceof Error && 'code' in err && err.code === 'not_found') family = undefined;
        else error = 'Decision undone, but related copies could not be refreshed. Reload the message to see current archive evidence.';
      }
    });
  }
  async function loadHistory() {
    await action(async current => {
      const result = data(await listEmailFamilyDecisions({ message_id: messageID }, { ...client, signal: abort.signal }));
      if (current === generation) history = result;
    });
  }
  async function download() {
    if (!family) return;
    const revision = family.revision;
    await action(async current => {
      try {
        const blob = data(await exportEmailFamily({ id: messageID }, { expected_family_revision: revision }, { ...client, signal: abort.signal }));
        if (current !== generation) return;
        const url = URL.createObjectURL(blob);
        const anchor = document.createElement('a'); anchor.href = url; anchor.download = `email-family-${messageID}.zip`; anchor.click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      } catch (err) {
        if (!(err instanceof Error && 'code' in err && err.code === 'family_changed')) throw err;
        const refreshed = data(await getEmailFamily({ id: messageID }, undefined, { ...client, signal: abort.signal }));
        if (current === generation) {
          family = refreshed;
          selected = [];
          pending = undefined; last = undefined; history = undefined;
          error = 'Archive evidence changed. Related copies were refreshed; review them before retrying.';
        }
      }
    });
  }
</script>

<section class="related-copies" aria-label="Related archived copies">
  {#if error}<p role="alert">{error}</p>{/if}
  {#if family}
    {#if showPanel}
      <Button size="sm" surface="soft" label={`Related copies (${family.members.length}${family.truncated ? '+' : ''})`} ariaExpanded={open} onclick={() => open = !open} />
    {/if}
    <Button size="sm" surface="soft" label="Link related copies" disabled={busy} onclick={() => { linking = !linking; open = true; pending = undefined; }} />
    {#if open}
      <p>Handling: <strong>{family.handling.state}</strong></p>
      {#each family.handling.reasons as value}<p>{reason(value)}</p>{/each}
      {#if family.truncated}<p role="status">Only part of this family is shown. Decisions are unavailable; ZIP exports are marked incomplete.</p>{/if}
      {#each groups as [account, members]}
        <fieldset disabled={busy}><legend>{account}</legend>
          {#each members as member (member.message_id)}
            <div class="copy"><Checkbox ariaLabel={`Select copy ${member.message_id}`} checked={selected.includes(member.message_id)} onchange={() => toggle(member.message_id)} />
              <a href={`/messages/${member.message_id}`}>{member.subject || `Message ${member.message_id}`}</a>
              <span>{member.sent_at || member.received_at || member.internal_date || 'Date unavailable'}</span>
              <span>{member.positive_sent ? 'Sent' : ''}{member.draft ? ' Draft' : ''}{member.local_handled ? ' Locally handled' : ''}</span>
              <small>{member.evidence_kinds?.map(reason).join(', ')} · Extraction: {reason(member.linkage.outcome)}</small>
            </div>
          {/each}
        </fieldset>
      {/each}
      {#if linking}<label>Other archive message IDs<TextInput ariaLabel="Other archive message IDs" bind:value={otherIDs} oninput={() => pending = undefined} placeholder="For example: 42, 57" disabled={busy} /></label><p>Links connect archived copies without changing provider threads.</p>{/if}
      <div class="actions">
        <Button size="sm" label="Preview handled" disabled={busy || family.truncated || !selected.length} onclick={() => preview('handled')} />
        {#if linking}<Button size="sm" label="Preview link" disabled={busy || family.truncated} onclick={() => preview('link')} />{/if}
        <Button size="sm" surface="soft" label="Download originals ZIP" disabled={busy} onclick={download} />
        <Button size="sm" surface="soft" label="Decision history" disabled={busy} onclick={loadHistory} />
      </div>
      {#if pending}
        <div class="preview" role="region" aria-label="Decision preview">
          <p>{pending.input.kind === 'handled' ? 'Mark only these copies as handled' : 'Link these copies'}: {pending.input.message_ids.join(', ')}</p>
          <p>Projected handling: {pending.preview.family.handling.state}. {pending.preview.family.members.length} copies in the preview.</p>
          {#each pending.preview.family.handling.reasons as value}<p>{reason(value)}</p>{/each}
          <Button label="Confirm decision" disabled={busy || pending.preview.family.truncated} onclick={confirm} />
          <Button surface="soft" label="Cancel preview" disabled={busy} onclick={() => pending = undefined} />
        </div>
      {/if}
      {#if last}<div role="status">Local decision saved. <Button size="sm" surface="soft" label="Undo last decision" disabled={busy} onclick={() => undo(last!.decision)} /></div>{/if}
      {#if history}
        <h3>Recent decisions</h3>
        {#if !history.length}<p>No local decisions for this copy.</p>{/if}
        {#each history as decision (decision.id)}
          <p>{decision.kind === 'handled' ? 'Handled exact copies' : 'Linked copies'} {decision.message_ids.join(', ')} · {decision.state}
            {#if decision.state === 'active'}<Button size="sm" surface="soft" label={`Undo decision ${decision.id}`} disabled={busy} onclick={() => undo(decision)} />{/if}
          </p>
        {/each}
        <p>Deduplication keeps decisions on the surviving copy; undoing deduplication does not move them back.</p>
      {/if}
    {/if}
  {/if}
</section>

<style>
  .related-copies { overflow-wrap: anywhere; min-width: 0; margin: .75rem 0; padding: .5rem; border-block: 1px solid var(--border-default); }
  fieldset { min-inline-size: 0; margin: .75rem 0; border: 1px solid var(--border-default); }
  .copy { display: flex; flex-wrap: wrap; gap: .5rem; align-items: center; padding: .4rem 0; }
  .copy small { width: 100%; }
  .actions { display: flex; flex-wrap: wrap; gap: .5rem; margin-block: .75rem; }
  .preview { padding: .75rem; border: 1px solid var(--border-default); }
</style>
