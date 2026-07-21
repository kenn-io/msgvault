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
  /* Deterministic muted hue per identity: fixed low saturation and
   * theme-appropriate lightness so the tiles sit quietly in both themes;
   * the glyph is a deeper shade of the same hue. Presentation-only. */
  .identity-avatar {
    display: inline-flex;
    flex: none;
    align-items: center;
    justify-content: center;
    border-radius: 27%;
    background: hsl(var(--avatar-hue, 210) 30% 87%);
    color: hsl(var(--avatar-hue, 210) 45% 29%);
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
    background: hsl(var(--avatar-hue, 210) 22% 27%);
    color: hsl(var(--avatar-hue, 210) 52% 80%);
  }
</style>
