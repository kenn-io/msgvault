import { render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';

import RowKind from './RowKind.svelte';

describe('RowKind', () => {
  it.each([
    ['conversation', 'sms', 'Conversation item'],
    ['event', 'calendar_event', 'Calendar event'],
    ['meeting', 'meeting_transcript', 'Meeting item'],
    ['email', 'unknown', 'Email item'],
    ['unknown', 'unknown', 'Archive item']
  ])('presents server kind %s before message type %s', (kind, messageType, label) => {
    render(RowKind, { kind, messageType });
    expect(screen.getByLabelText(label)).toBeDefined();
  });
});
