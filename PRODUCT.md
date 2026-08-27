# Message Vault

## Register

product

## Users

People managing a large, long-lived personal message archive. They work in dense search, grouping, reading, file, relationship, source, deletion, and settings views. The interface must support sustained keyboard and pointer use without hiding archive state or destructive actions.

## Product Purpose

Message Vault stores email and other message sources locally, makes the archive searchable, and gives users a dependable way to inspect, organize, export, and delete archived material. The browser application exposes the archive through the same daemon that owns the API and must remain useful across local and remote deployments.

## Brand Personality

Quiet, precise, and trustworthy. Controls should feel familiar to users of the other kenn tools. The archive and its current state matter more than decoration.

## Anti-references

- A one-off component library that makes standard controls behave differently from the rest of the kenn stack.
- A spacious card dashboard that hides dense archive data behind oversized chrome.
- A mail-client imitation that obscures the analytical model or makes deletion feel casual.

## Design Principles

- Use Kit UI for shared controls, theme behavior, overlays, tables, and layout mechanics.
- Keep product-specific archive views local, but compose them from the shared component vocabulary.
- Preserve density, keyboard navigation, URL-backed context, and explicit terminal states.
- Make source authority, loading, errors, and destructive actions visible before the user acts.
- Prefer stable, familiar interaction patterns over local visual invention.

## Accessibility & Inclusion

Maintain keyboard access, visible focus, semantic labels, reduced-motion behavior, light and dark themes, high contrast where Kit supports it, and automated accessibility coverage. Controls must not rely on color alone to communicate state.
