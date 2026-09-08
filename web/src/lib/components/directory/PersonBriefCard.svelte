<script lang="ts">
  import { Button, Card, Checkbox, Spinner, TextInput, Toggle } from '@kenn-io/kit-ui';
  import { onDestroy, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import { PersonBriefController } from '../../directory/person-brief-controller.svelte';
  import type { PersonBriefVersionView } from '../../directory/person-brief-controller.svelte';

  interface Props {
    client: APIClient;
    personID: number;
    onAnnounce?: (message: string) => void;
  }

  let { client, personID, onAnnounce = () => undefined }: Props = $props();
  const controller = new PersonBriefController(untrack(() => client));
  let root = $state<HTMLElement>();
  let alsoTrack = $state(false);
  let rejectReason = $state('');
  let expandedSentence = $state<string | null>(null);
  // The switch is bound, not read-only, because a native checkbox keeps
  // whatever the owner clicked. Binding lets a refused change snap it back to
  // the state the daemon still holds without replacing the focused control.
  let enrolledSwitch = $state(false);

  const headingID = $derived(`person-${personID}-brief-heading`);
  const expansionID = $derived(`person-${personID}-brief-expansion`);
  const busy = $derived(
    controller.enrollmentLoading || controller.briefLoading || controller.versionsLoading
  );
  const actionsDisabled = $derived(controller.pending !== null || controller.briefLoading);

  $effect.pre(() => {
    const nextPersonID = personID;
    alsoTrack = false;
    rejectReason = '';
    expandedSentence = null;
    untrack(() => {
      void controller.setPerson(nextPersonID);
    });
  });

  // Mirror the loaded enrollment onto the switch. The switch is an input whose
  // state the browser owns, so the controller's state has to be pushed to it.
  $effect(() => {
    const enrolled = controller.enrollment?.enrolled ?? false;
    untrack(() => {
      enrolledSwitch = enrolled;
    });
  });

  onDestroy(() => controller.destroy());

  function statusLabel(status: string): string {
    switch (status) {
      case 'current':
        return 'Current';
      case 'superseded':
        return 'Superseded';
      case 'rejected':
        return 'Rejected';
      default:
        return 'Unknown status';
    }
  }

  // Directness is recorded relative to the person the brief is about, never the
  // archive owner: a v1 brief packet holds only the person's own messages, so
  // direct-self is something they wrote on an authenticated source and
  // direct-other is something they said in a transcribed meeting.
  function directnessLabel(directness: string): string {
    switch (directness) {
      case 'direct-self':
        return 'This person wrote it';
      case 'direct-other':
        return 'This person said it (meeting transcript)';
      case 'indirect':
        return 'Indirect';
      default:
        return 'Attribution unstated';
    }
  }

  function versionSummary(version: PersonBriefVersionView): string {
    return `Version ${version.version} · ${statusLabel(version.status)}`;
  }

  function toggleSentence(key: string): void {
    expandedSentence = expandedSentence === key ? null : key;
  }

  async function finishInContext(contextToken: number, announcement: string | null): Promise<void> {
    if (!controller.isContextCurrent(contextToken)) return;
    await tick();
    if (!controller.isContextCurrent(contextToken)) return;
    if (announcement) onAnnounce(announcement);
    const target =
      root?.querySelector<HTMLElement>('input:not(:disabled)') ?? root?.querySelector<HTMLElement>('h3');
    if (target?.isConnected) target.focus();
  }

  async function changeEnrollment(enrolled: boolean): Promise<void> {
    const contextToken = controller.contextToken;
    expandedSentence = null;
    const outcome = await controller.setEnrolled(enrolled, alsoTrack);
    if (!controller.isContextCurrent(contextToken)) return;
    if (outcome.kind !== 'confirmed') {
      enrolledSwitch = controller.enrollment?.enrolled ?? false;
      return;
    }
    // The checkbox only exists while the person is unenrolled, so a stale true
    // would silently re-track them if they were unenrolled and enrolled again.
    alsoTrack = false;
    await finishInContext(contextToken, controller.announcement);
  }

  async function rejectBrief(): Promise<void> {
    const contextToken = controller.contextToken;
    const outcome = await controller.reject(rejectReason);
    if (outcome.kind !== 'confirmed') return;
    if (!controller.isContextCurrent(contextToken)) return;
    rejectReason = '';
    if (controller.announcement) onAnnounce(controller.announcement);
  }

  async function generateBrief(): Promise<void> {
    const contextToken = controller.contextToken;
    expandedSentence = null;
    const outcome = await controller.generate();
    if (outcome.kind !== 'confirmed') return;
    if (!controller.isContextCurrent(contextToken)) return;
    if (controller.announcement) onAnnounce(controller.announcement);
  }

  async function toggleVersions(): Promise<void> {
    if (controller.versionsShown) {
      controller.hideVersions();
      return;
    }
    await controller.showVersions();
  }
</script>

<section bind:this={root} class="brief" aria-labelledby={headingID}>
  <Card level="default" padding="sm">
    <div class="brief-content">
      <div class="heading-row">
        <div>
          <h3 id={headingID} tabindex="-1">Last time we talked</h3>
          <p>
            Catch up before your next conversation with a summary of what this person recently shared
            in supported chat and text messages. Expand a sentence to see its sources.
          </p>
        </div>
        {#if busy}
          <span class="working" aria-label="Loading the brief" aria-busy="true">
            <Spinner size={14} label="Loading the brief" />
          </span>
        {/if}
      </div>

      {#if controller.enrollmentError}
        <div class="notice notice--error" role="alert">
          <span>{controller.enrollmentError}</span>
          <Button
            size="sm"
            label="Retry brief enrollment"
            disabled={controller.enrollmentLoading || controller.pending !== null}
            onclick={() => void controller.retryEnrollment()}
          />
        </div>
      {/if}

      {#if controller.enrollment}
        {@const enrollment = controller.enrollment}
        <div class="enrollment-row">
          <Toggle
            bind:checked={enrolledSwitch}
            ariaLabel="Enroll this person in briefs"
            disabled={controller.enrollmentLoading || controller.pending !== null}
            onchange={(checked) => void changeEnrollment(checked)}
          />
          {#if !enrollment.enrolled}
            <Checkbox
              checked={alsoTrack}
              label="Also track this person"
              disabled={controller.pending !== null}
              onchange={(checked) => (alsoTrack = checked)}
            />
          {:else if enrollment.enabled_at}
            <span class="muted">
              Enrolled since
              <time datetime={enrollment.enabled_at}>{enrollment.enabled_at}</time>
            </span>
          {/if}
        </div>
      {/if}

      {#if controller.enrollment && !controller.enrollment.enrolled}
        <p class="muted">This person is not enrolled, so no brief is generated or shown.</p>
      {:else if controller.enrollment}
        {#if controller.briefError}
          <div class="notice notice--error" role="alert">
            <span>{controller.briefError}</span>
            <Button
              size="sm"
              label="Retry the brief"
              disabled={controller.briefLoading || controller.pending !== null}
              onclick={() => void controller.retryBrief()}
            />
          </div>
        {/if}

        {#if controller.actionError}
          <div class="notice notice--error" role="alert">{controller.actionError}</div>
        {/if}

        {#if controller.pending === 'generate'}
          <p class="working" role="status" aria-busy="true">
            <Spinner size={14} label="Generating a brief" />
            Generating a brief. This spends provider budget.
          </p>
        {:else if controller.runOutcome}
          <p class="run-outcome" role="status">{controller.runOutcome}</p>
        {/if}

        {#if controller.brief}
          {@const brief = controller.brief}
          <p class="version-line">
            Version {brief.version} · {statusLabel(brief.status)} · generated
            <time datetime={brief.generated_at}>{brief.generated_at}</time>
            {#if brief.rejected_at}
              · rejected <time datetime={brief.rejected_at}>{brief.rejected_at}</time>
              {#if brief.rejected_reason}({brief.rejected_reason}){/if}
            {/if}
          </p>

          {#if brief.sentences.length > 0}
            <p class="paragraph">
              {#each brief.sentences as sentence (sentence.key)}
                <button
                  type="button"
                  class="sentence"
                  aria-expanded={expandedSentence === sentence.key}
                  aria-controls={expandedSentence === sentence.key ? expansionID : undefined}
                  onclick={() => toggleSentence(sentence.key)}>{sentence.text}</button
                >
              {/each}
            </p>

            {#if expandedSentence !== null}
              {@const sentence = brief.sentences.find((item) => item.key === expandedSentence)}
              {#if sentence}
                <div id={expansionID} class="expansion">
                  {#if sentence.detail}
                    <p class="detail">{sentence.detail}</p>
                  {:else}
                    <p class="muted">This brief version carries no structured item for that sentence.</p>
                  {/if}
                  {#if sentence.meta.length > 0}
                    <ul class="meta">
                      {#each sentence.meta as label}<li>{label}</li>{/each}
                    </ul>
                  {/if}
                  {#if sentence.evidence.length > 0}
                    <h4>Archive items this sentence cites</h4>
                    <ul class="evidence">
                      {#each sentence.evidence as item (item.ordinal)}
                        <li>
                          <strong>{item.source_ref}</strong>
                          <span><time datetime={item.event_time}>{item.event_time}</time></span>
                          <span>{directnessLabel(item.directness)}</span>
                          {#if !item.supported}
                            <span class="unsupported">Supporting source is no longer available</span>
                          {/if}
                        </li>
                      {/each}
                    </ul>
                  {:else}
                    <h4>Archive items this brief cites</h4>
                    <p class="muted">
                      Cited by the brief as a whole. Msgvault does not attribute them to a single
                      sentence.
                    </p>
                    {#if brief.evidence.length > 0}
                      <ul class="evidence">
                        {#each brief.evidence as item (item.ordinal)}
                          <li>
                            <strong>{item.source_ref}</strong>
                            <span><time datetime={item.event_time}>{item.event_time}</time></span>
                            <span>{directnessLabel(item.directness)}</span>
                            {#if !item.supported}
                              <span class="unsupported">Supporting source is no longer available</span>
                            {/if}
                          </li>
                        {/each}
                      </ul>
                    {:else}
                      <p class="muted">This brief version cites no archive item.</p>
                    {/if}
                  {/if}
                </div>
              {/if}
            {/if}
          {:else}
            <p class="paragraph paragraph--plain">{brief.rendered_text}</p>
            <p class="muted">Sentence expansion is unavailable for this brief version.</p>
          {/if}

          {#if brief.dropped_item_count > 0}
            <p class="muted">
              {brief.dropped_item_count}
              {brief.dropped_item_count === 1 ? 'item was dropped' : 'items were dropped'} for citing
              evidence the archive packet does not contain.
            </p>
          {/if}

          <div class="actions">
            <TextInput
              bind:value={rejectReason}
              size="sm"
              ariaLabel="Why this brief is wrong (optional)"
              placeholder="Why this brief is wrong (optional)"
              disabled={actionsDisabled}
            />
            <Button
              size="sm"
              tone="danger"
              label="Reject this brief version"
              disabled={actionsDisabled}
              onclick={() => void rejectBrief()}
            />
            <Button
              size="sm"
              label="Regenerate this brief"
              disabled={actionsDisabled}
              onclick={() => void generateBrief()}
            />
          </div>
        {:else if controller.briefMissing}
          <p>No brief yet.</p>
          <div class="actions">
            <Button
              size="sm"
              label="Generate a brief now"
              disabled={actionsDisabled}
              onclick={() => void generateBrief()}
            />
          </div>
        {/if}

        <div class="history">
          <div class="history-heading">
            <h4>Version history</h4>
            <Button
              size="sm"
              label={controller.versionsShown ? 'Hide brief version history' : 'Show brief version history'}
              ariaExpanded={controller.versionsShown}
              disabled={controller.versionsLoading}
              onclick={() => void toggleVersions()}
            />
          </div>

          {#if controller.versionsError}
            <div class="notice notice--error" role="alert">
              <span>{controller.versionsError}</span>
              <Button
                size="sm"
                label="Retry brief version history"
                disabled={controller.versionsLoading}
                onclick={() => void controller.retryVersions()}
              />
            </div>
          {/if}

          {#if controller.versionsShown && controller.versions.length > 0}
            <ul class="version-list" aria-label="Brief version history">
              {#each controller.versions as version (version.version)}
                <li>
                  <strong>{versionSummary(version)}</strong>
                  <span>Generated <time datetime={version.generated_at}>{version.generated_at}</time></span>
                  {#if version.superseded_at}
                    <span>Superseded <time datetime={version.superseded_at}>{version.superseded_at}</time></span>
                  {/if}
                  {#if version.rejected_at}
                    <span>Rejected <time datetime={version.rejected_at}>{version.rejected_at}</time></span>
                  {/if}
                  {#if version.rejected_reason}<span>Reason: {version.rejected_reason}</span>{/if}
                </li>
              {/each}
            </ul>
          {:else if controller.versionsShown && !controller.versionsLoading && !controller.versionsError}
            <p class="muted">No brief versions have been stored for this person.</p>
          {/if}
        </div>
      {/if}
    </div>
  </Card>
</section>

<style>
  .brief, .brief-content, .expansion, .history { display: grid; gap: var(--space-3); min-width: 0; }
  .heading-row, .enrollment-row, .notice, .actions, .history-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: var(--space-3); }
  .heading-row > div, .history-heading { align-items: center; }
  .heading-row > div { display: grid; gap: var(--space-1); }
  .enrollment-row, .actions { align-items: center; justify-content: flex-start; flex-wrap: wrap; }
  h3, h4, p, ul { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  h4 { font-size: var(--font-size-sm); }
  .heading-row p, .muted, .version-line, .meta, .evidence span { color: var(--text-muted); font-size: var(--font-size-sm); }
  .working { display: flex; align-items: center; gap: var(--space-2); }
  .notice { align-items: center; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .notice--error { border-color: var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }
  .run-outcome { padding: var(--space-2) var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .paragraph { line-height: 1.6; overflow-wrap: anywhere; }
  .paragraph--plain { padding: var(--space-1) 0; }
  .sentence { display: inline; margin: 0 2px 0 0; padding: 0 2px; border: 0; border-radius: var(--radius-sm); background: transparent; color: var(--text-primary); font: inherit; text-align: left; cursor: pointer; }
  .sentence:hover, .sentence[aria-expanded="true"] { background: var(--bg-surface-hover); }
  .sentence[aria-expanded="true"] { box-shadow: inset 0 -1px 0 var(--border-strong); }
  .expansion { padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-inset); }
  .detail { font-weight: 600; }
  .meta, .evidence { display: grid; gap: var(--space-1); padding: 0; list-style: none; }
  .evidence li, .version-list li { display: grid; align-content: start; gap: var(--space-1); min-width: 0; padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-md); overflow-wrap: anywhere; }
  .unsupported { color: var(--status-error-ink) !important; }
  .version-list { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--space-2); padding: 0; list-style: none; }
  .version-list span { color: var(--text-muted); font-size: var(--font-size-sm); }

  @media (max-width: 760px) {
    .heading-row, .notice, .history-heading { align-items: stretch; flex-direction: column; }
    .version-list { grid-template-columns: minmax(0, 1fr); }
  }
</style>
