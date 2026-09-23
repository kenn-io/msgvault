import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { AttributeValue, PersonAttributeGroup } from '../../api/generated/models';
import AttributeSummary from './AttributeSummary.svelte';

afterEach(() => cleanup());

function group(label: string, value: AttributeValue | undefined, sensitive = false): PersonAttributeGroup {
  return {
    definition: { universal_id: `u-${label}`, slug: label.toLowerCase(), label, is_sensitive: sensitive, display_order: 0 },
    current: value === undefined ? [] : [{ value }],
    history: []
  } as unknown as PersonAttributeGroup;
}

describe('AttributeSummary', () => {
  it('lists only current values, formats typed values and choice labels, and conceals sensitive values', async () => {
    const onEdit = vi.fn();
    const status = group('Status', { type: 'text', text: 'active' });
    status.definition.options = { choices: [{ value: 'active', label: 'Active contact' }] };
    const subscribed = group('Subscribed', { type: 'boolean', boolean: true });
    subscribed.definition.options = { choices: [{ value: 'true', label: 'Opted in' }] };
    render(AttributeSummary, {
      groups: [
        group('Birthday', { type: 'date', date: '1990-01-01' }),
        subscribed,
        status,
        group('Employer', undefined),
        group('Health', { type: 'text', text: 'synthetic private value' }, true)
      ],
      onEdit
    });
    const region = screen.getByRole('region', { name: 'Attributes summary' });
    expect(region.textContent).toContain('Birthday');
    expect(region.textContent).toContain('1990-01-01');
    expect(region.textContent).toContain('Subscribed');
    expect(region.textContent).toContain('Opted in');
    expect(region.textContent).toContain('Active contact');
    expect(region.textContent).not.toContain('active');
    expect(region.textContent).not.toContain('Employer');
    expect(region.textContent).toContain('Health');
    expect(region.textContent).toContain('concealed');
    expect(region.textContent).not.toContain('synthetic private value');
    await fireEvent.click(screen.getByRole('button', { name: 'Edit attributes' }));
    expect(onEdit).toHaveBeenCalledOnce();
  });

  it('renders nothing when no attribute has a current value', () => {
    const { container } = render(AttributeSummary, { groups: [group('Employer', undefined)] });
    expect(container.querySelector('section')).toBeNull();
  });
});
