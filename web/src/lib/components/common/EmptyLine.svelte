<script lang="ts">
  // setupLabel names the button when several Set up… actions share a page, so each stays distinct to a screen reader.
  let { message, onsetup, setupLabel }: { message: string; onsetup?: () => void; setupLabel?: string } = $props();
</script>

<p>{message}{#if onsetup}{' '}<button class="kit-control-states" type="button" aria-label={setupLabel} onclick={onsetup}>Set up…</button>{/if}</p>

<style>
  /* A Meta line; Set up… is a text action inside the sentence, so it keeps the sentence's line. */
  button { padding: 0; border: 0; background: none; color: var(--link-ink); font: inherit; cursor: pointer; }
  p { margin: 0; color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); text-align: left; }
</style>
