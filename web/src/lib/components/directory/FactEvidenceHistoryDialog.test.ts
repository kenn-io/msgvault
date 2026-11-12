import { cleanup, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
import FactEvidenceHistoryDialog from './FactEvidenceHistoryDialog.svelte';

afterEach(() => cleanup());

describe('FactEvidenceHistoryDialog', () => {
  it('shows source history reasons', () => {
    const controller = new FactLedgerController(createAPIClient(vi.fn<typeof fetch>()));
    controller.historyOpen = true;
    controller.history.rows = [
      { supported: false, reasonLabel: 'Source deleted', createdAt: '2026-08-01' },
      { supported: true, reasonLabel: 'Imported again', createdAt: '2026-08-02' }
    ];
    render(FactEvidenceHistoryDialog, { controller, onClose: () => {} });

    const rows = screen.getAllByRole('listitem');
    expect(rows[0]!.textContent).toContain('Source deleted');
    expect(rows[1]!.textContent).toContain('Imported again');
  });
});
