<script lang="ts">
  import { identityHue, initialsFor } from '../../util/identity';

  interface Props {
    /** Visible display label the initials derive from. */
    label: string;
    /** Stable identity key the hue derives from; falls back to the label. */
    seed?: string;
    /** Domains render a single letter in a squarer tile so people and
     * domains read differently at a glance. */
    shape?: 'person' | 'domain';
    size?: number;
  }

  let { label, seed = undefined, shape = 'person', size = 28 }: Props = $props();

  const hue = $derived(identityHue(seed ?? label));
  const glyph = $derived(
    shape === 'domain' ? (label.trim()[0] ?? '?').toUpperCase() : initialsFor(label)
  );
</script>

<span
  class="identity-avatar"
  class:identity-avatar--domain={shape === 'domain'}
  style:--avatar-hue={hue}
  style:width={`${size}px`}
  style:height={`${size}px`}
  style:font-size={`${Math.round(size * 0.39)}px`}
  aria-hidden="true"
>{glyph}</span>

<style>
  /* Deterministic identity hue with a hollow touch: a ~13% tint of the hue
   * as the fill, the initials in a deeper shade of the same hue. Rows stay
   * text-first — the tile marks identity without competing with content.
   * Presentation-only. */
  .identity-avatar {
    display: inline-flex;
    flex: none;
    align-items: center;
    justify-content: center;
    border-radius: 27%;
    background: hsl(var(--avatar-hue, 210) 55% 45% / 0.13);
    color: hsl(var(--avatar-hue, 210) 40% 36%);
    font-family: var(--font-sans);
    font-weight: 600;
    letter-spacing: 0.02em;
    line-height: 1;
    user-select: none;
  }

  .identity-avatar--domain {
    border-radius: 12%;
  }

  :global([data-theme='dark']) .identity-avatar {
    background: hsl(var(--avatar-hue, 210) 55% 65% / 0.14);
    color: hsl(var(--avatar-hue, 210) 45% 74%);
  }
</style>
