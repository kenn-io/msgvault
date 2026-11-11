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
    onaction?: () => void;
  }

  let { tone = 'error', message, children = undefined, details = undefined, actionLabel = undefined, onaction = undefined }: Props = $props();
</script>

<div class="inline-notice">
  <Notice {tone} {actionLabel} {onaction}>
    <p>{message}</p>
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
  .inline-notice :global(.kit-disclosure .kit-disclosure__summary.kit-disclosure__summary) { font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  p { margin: 0; }
  pre { max-height: 16rem; margin: 0; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; font-size: var(--type-small-size); line-height: var(--type-small-line); }
</style>
