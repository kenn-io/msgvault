<script lang="ts">
  import type { Snippet } from 'svelte';
  import { Disclosure, Notice, type NoticeTone } from '@kenn-io/kit-ui';

  interface Props {
    tone?: NoticeTone;
    /** One sentence that says what happened. */
    message: string;
    children?: Snippet;
    /** The original error, folded under Details inside the notice. */
    details?: string;
    actionLabel?: string;
    actionDisabled?: boolean;
    onaction?: () => void;
    /** A second text action, left of the first, for a notice that can also be dismissed. */
    secondaryActionLabel?: string;
    onsecondaryaction?: () => void;
  }

  let { tone = 'error', message, children = undefined, details = undefined, actionLabel = undefined, actionDisabled = false, onaction = undefined, secondaryActionLabel = undefined, onsecondaryaction = undefined }: Props = $props();
  let notice: HTMLDivElement;
  $effect(() => {
    // Notice creates its action only when both props are present.
    void actionLabel;
    void onaction;
    const button = notice.querySelector<HTMLButtonElement>('.kit-notice > button');
    if (button) button.disabled = actionDisabled;
  });
</script>

<div class="inline-notice" bind:this={notice}>
  <Notice {tone} {actionLabel} {onaction}>
    {#if secondaryActionLabel && onsecondaryaction}
      <div class="inline-notice__lead"><p>{message}</p><button class="kit-control-states" type="button" disabled={actionDisabled} onclick={onsecondaryaction}>{secondaryActionLabel}</button></div>
    {:else}<p>{message}</p>{/if}
    {@render children?.()}
    {#if details}<Disclosure variant="plain" title="Details"><pre>{details}</pre></Disclosure>{/if}
  </Notice>
</div>

<style>
  /* A box padded 12, the mark 8 from Meta text, and the action a text action with a 28 target at the right of the
   * sentence, 12 from the box's edge like the mark. The mark and the sentence pad down to center their line on it.
   * Classes repeat to outrank Notice's scoped rules, which are as specific as a single class here. */
  .inline-notice { --notice-line-offset: calc((var(--control-h) - var(--type-meta-line)) / 2); min-width: 0; }
  /* On a phone too the action stays at the right of the sentence, above Details, where Notice would wrap it under them. */
  .inline-notice :global(.kit-notice.kit-notice) { grid-template-columns: auto minmax(0, 1fr) auto; padding: var(--space-box); gap: var(--space-group); }
  /* kit-ui-check-ignore: Notice has no class prop for its tone heading, so the kit-sr-only recipe hides it from view while screen readers keep it. */
  .inline-notice :global(.kit-notice .kit-notice__tone.kit-notice__tone) { position: absolute; width: 1px; height: 1px; margin: -1px; padding: 0; overflow: hidden; clip-path: inset(50%); white-space: nowrap; border: 0; }
  .inline-notice :global(.kit-notice .kit-notice__mark.kit-notice__mark) { margin-top: calc((var(--type-meta-line) - 8px) / 2); }
  .inline-notice :global(.kit-notice:has(> button) .kit-notice__mark.kit-notice__mark) { margin-top: calc(var(--notice-line-offset) + (var(--type-meta-line) - 8px) / 2); }
  .inline-notice :global(.kit-notice:has(> button) .kit-notice__content) { padding-top: var(--notice-line-offset); }
  .inline-notice :global(.kit-notice .kit-notice__body.kit-notice__body) { display: grid; gap: var(--space-group); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .inline-notice :global(.kit-notice.kit-notice > button) { grid-column: auto; justify-self: auto; min-height: var(--control-h); margin: 0; padding: 0; font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .inline-notice :global(.kit-notice.kit-notice > button:hover) { background: transparent; text-decoration: underline; }
  /* A second action sits in the sentence's row, one group gap left of the first, styled as its twin and raised back onto the action line. */
  .inline-notice__lead { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: start; gap: var(--space-group); }
  .inline-notice__lead button { min-height: var(--control-h); margin: calc(-1 * var(--notice-line-offset)) 0 0; padding: 0; background: transparent; border: 0; border-radius: var(--radius-sm); color: var(--text-primary); cursor: pointer; font: inherit; font-size: var(--type-meta-size); font-weight: var(--font-weight-semibold); line-height: var(--type-meta-line); }
  .inline-notice__lead button:hover:not(:disabled) { text-decoration: underline; }
  .inline-notice__lead button:focus-visible { outline: var(--focus-ring); outline-offset: 2px; }
  .inline-notice :global(.kit-disclosure .kit-disclosure__summary.kit-disclosure__summary) { font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  p { margin: 0; }
  pre { max-height: 16rem; margin: 0; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; font-size: var(--type-small-size); line-height: var(--type-small-line); }
</style>
