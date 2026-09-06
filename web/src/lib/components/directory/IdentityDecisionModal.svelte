<script lang="ts">
  import { appShortcuts, Button, Modal } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';

  import type {
    DirectoryReviewContextSnapshot,
    DirectoryReviewController,
    IdentityMatchCandidate,
    PersonMergeRequiredError
  } from '../../directory/review-controller.svelte';

  interface Props {
    controller: DirectoryReviewController;
    candidate: IdentityMatchCandidate;
    decision: 'accept' | 'reject';
    reviewContext: DirectoryReviewContextSnapshot;
    onClose: () => void;
    onContextInvalidated: () => void;
    onResolveMerge?: (conflict: PersonMergeRequiredError) => void;
  }

  let {
    controller,
    candidate,
    decision,
    reviewContext,
    onClose,
    onContextInvalidated,
    onResolveMerge = undefined
  }: Props = $props();
  let submitting = $state(false);
  let error = $state<string | null>(null);
  let conflict = $state<PersonMergeRequiredError | null>(null);
  let releaseShortcutScope: (() => void) | undefined;

  const pending = $derived(submitting || controller.isDecisionPending(candidate.id));
  const title = $derived(decision === 'accept' ? 'Link identities' : 'Keep separate');
  const draft = $derived(controller.getDecisionDraft(candidate.id));

  onMount(() => {
    releaseShortcutScope = appShortcuts.pushScope('identity-decision-modal');
  });

  onDestroy(() => releaseShortcutScope?.());

  function updateDraft(value: string): void {
    controller.setDecisionDraft(candidate.id, value);
  }

  function requestClose(): void {
    if (!controller.isReviewContextCurrent(reviewContext)) {
      onContextInvalidated();
      return;
    }
    if (pending) return;
    onClose();
  }

  async function submit(): Promise<void> {
    if (!controller.isReviewContextCurrent(reviewContext)) {
      onContextInvalidated();
      return;
    }
    if (pending || conflict) return;
    submitting = true;
    error = null;
    try {
      const result = decision === 'accept'
        ? await controller.acceptIdentity(candidate.id, undefined, reviewContext)
        : await controller.rejectIdentity(candidate.id, undefined, reviewContext);
      if (result.ok) {
        onClose();
      } else if (result.kind === 'merge_required') {
        conflict = result.conflict;
      } else {
        error = result.message;
      }
    } finally {
      submitting = false;
    }
  }

  function profileLabel(profile: NonNullable<PersonMergeRequiredError['profiles']>[number]): string {
    return `${profile.person.display_name?.trim() || `Person ${profile.person.id}`} (Person ${profile.person.id})`;
  }
</script>

<Modal
  {title}
  ariaLabel={title}
  closeLabel="Close identity decision"
  closable={!pending}
  closeOnOverlayClick={!pending}
  onclose={requestClose}
  maxWidth="min(560px, calc(100vw - 32px))"
>
  <div class="decision" aria-busy={pending}>
    <p class="candidate-context">
      <strong>{candidate.left_kind} / {candidate.left_id}</strong>
      <span aria-hidden="true">↔</span>
      <strong>{candidate.right_kind} / {candidate.right_id}</strong>
    </p>

    {#if decision === 'accept'}
      <p>Link these identities only when the supplied evidence shows they belong to the same person.</p>
    {:else}
      <p>Keep these identities separate when the supplied evidence does not establish that they belong to the same person.</p>
    {/if}

    <label>
      <span>Decision notes</span>
      <textarea
        aria-label="Decision notes"
        rows="4"
        value={draft}
        disabled={pending || !!conflict}
        oninput={(event) => updateDraft(event.currentTarget.value)}
      ></textarea>
    </label>

    {#if conflict}
      <div class="merge-required" role="alert">
        <strong>An explicit merge is required before these identities can be linked.</strong>
        <p>The acceptance was recorded as a conflict and was not retried. Review both durable profiles before choosing a survivor.</p>
        <ul aria-label="Profiles requiring merge">
          {#each conflict.profiles ?? [] as profile (profile.person.id)}
            <li>
              <dl>
                <div><dt>Profile</dt><dd>{profileLabel(profile)}</dd></div>
                <div><dt>ETag</dt><dd><code>{profile.etag}</code></dd></div>
              </dl>
            </li>
          {/each}
        </ul>
      </div>
    {:else if error}
      <p class="decision-error" role="alert">{error}</p>
    {/if}
  </div>

  {#snippet footer()}
    <Button surface="soft" label="Cancel" disabled={pending} onclick={requestClose} />
    {#if conflict}
      <Button
        tone="info"
        surface="solid"
        label="Resolve merge"
        disabled={!onResolveMerge}
        title={onResolveMerge ? undefined : 'Merge resolution is installed in the next review-centre task.'}
        onclick={() => onResolveMerge?.(conflict!)}
      />
    {:else}
      <Button
        tone={decision === 'accept' ? 'info' : 'neutral'}
        surface="solid"
        label={title}
        disabled={pending}
        onclick={() => void submit()}
      />
    {/if}
  {/snippet}
</Modal>

<style>
  .decision { display: grid; gap: var(--space-4); min-width: min(28rem, calc(100vw - 64px)); }
  p, ul, dl, dd { margin: 0; }
  .candidate-context { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); color: var(--text-primary); }
  label { display: grid; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-size-sm); font-weight: var(--font-weight-medium, 500); }
  textarea { box-sizing: border-box; width: 100%; resize: vertical; padding: var(--space-3); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); color: var(--text-primary); font: inherit; line-height: 1.45; }
  textarea:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  textarea:disabled { opacity: var(--opacity-disabled); }
  .decision-error { color: var(--text-danger); }
  .merge-required { display: grid; gap: var(--space-2); padding: var(--space-4); border: var(--border-width) solid color-mix(in srgb, var(--accent-amber) 30%, var(--border-default)); border-radius: var(--radius-sm); background: color-mix(in srgb, var(--accent-amber) 9%, var(--bg-surface)); color: var(--text-secondary); }
  .merge-required strong { color: color-mix(in srgb, var(--accent-amber) 72%, var(--text-primary)); }
  .merge-required ul { display: grid; gap: var(--space-1); padding-left: var(--space-5); }
  .merge-required li dl { display: grid; gap: var(--space-2); }
  .merge-required li dl > div { display: grid; gap: var(--space-1); }
  .merge-required dt { color: var(--text-muted); font-size: var(--font-size-xs); }
  .merge-required dd { overflow-wrap: anywhere; }
</style>
