import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
import FactEvidenceHistoryDialog from './FactEvidenceHistoryDialog.svelte';

afterEach(() => cleanup());

describe('FactEvidenceHistoryDialog', () => {
  it('renders fixed support labels, neutralizes unknown reasons upstream, and is Escape dismissible', async () => {
    const controller = new FactLedgerController(createAPIClient(vi.fn<typeof fetch>()));
    controller.historyOpen = true;
    controller.history.rows = [
      { supported: false, reasonLabel: 'Source deleted', createdAt: '2026-08-01' },
      { supported: true, reasonLabel: 'Support status changed', createdAt: '2026-08-02' }
    ];
    const onClose = vi.fn(() => controller.closeEvidenceHistory());
    render(FactEvidenceHistoryDialog, { controller, onClose });

    const dialog = screen.getByRole('dialog', { name: 'Evidence support history' });
    expect(screen.getByText('Unsupported')).toBeDefined();
    expect(screen.getByText('Source deleted')).toBeDefined();
    expect(screen.getByText('Support status changed')).toBeDefined();
    expect(screen.getByRole('button', { name: 'First support-history page' })).toHaveProperty('disabled', true);
    await fireEvent.keyDown(dialog, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledOnce();
  });
});
