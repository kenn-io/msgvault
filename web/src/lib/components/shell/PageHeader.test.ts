import { render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { describe, expect, it } from 'vitest';

import PageHeader from './PageHeader.svelte';

describe('PageHeader', () => {
  it('renders one level-one heading, the description, and actions', () => {
    const actions = createRawSnippet(() => ({ render: () => '<button>Refresh operations</button>' }));
    render(PageHeader, { title: 'Operations', description: 'Background work and its history.', actions });

    expect(screen.getByRole('heading', { level: 1, name: 'Operations' })).toBeTruthy();
    expect(screen.getByText('Background work and its history.')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Refresh operations' })).toBeTruthy();
  });

  it('renders the view row below the title', () => {
    const view = createRawSnippet(() => ({ render: () => '<div role="tablist">Views</div>' }));
    render(PageHeader, { title: 'Operations', view });

    const heading = screen.getByRole('heading', { level: 1, name: 'Operations' });
    const row = screen.getByRole('tablist');
    expect(heading.compareDocumentPosition(row) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('keeps a visually hidden title accessible', () => {
    render(PageHeader, { title: 'Operations', visuallyHiddenTitle: true });

    const heading = screen.getByRole('heading', { level: 1, name: 'Operations' });
    expect(heading.closest('.kit-sr-only')).toBeTruthy();
  });

  it('shows the title normally by default', () => {
    render(PageHeader, { title: 'Operations' });

    const heading = screen.getByRole('heading', { level: 1, name: 'Operations' });
    expect(heading.closest('.kit-sr-only')).toBeNull();
  });
});
