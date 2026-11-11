import Mic from '@lucide/svelte/icons/mic';
import { groupRowName } from './labels';
import { describe, expect, it } from 'vitest';

import {
  ExploreFilterDimension as FilterDimensions
} from '../api/generated/models/exploreFilterDimension';
import {
  entryKindPresentation,
  fileTypeLabel,
  filterDimensionLabel,
  filterValueLabel,
  preflightReasonLabel
} from './labels';

describe('explore labels', () => {
  it('names filter dimensions as chips and falls back to sentence case', () => {
    expect(filterDimensionLabel('source')).toBe('Source');
    expect(filterDimensionLabel('participant')).toBe('Person');
    expect(filterDimensionLabel('domain')).toBe('Domain');
    expect(filterDimensionLabel('message_type')).toBe('Message type');
    expect(filterDimensionLabel('mailing_list')).toBe('Mailing list');
    expect(filterDimensionLabel('made_up_dimension' as never)).toBe('Made up dimension');
  });

  it('shows account filter keys as the address or bucket they name', () => {
    expect(filterValueLabel('account', 'identity:7:d29ya0BleGFtcGxlLm9yZw')).toBe('work@example.org');
    expect(filterValueLabel('account', 'unattributed:7')).toBe('Unattributed');
    expect(filterValueLabel('account', 'mask@example.org')).toBe('mask@example.org');
    expect(filterValueLabel('account', 'identity:7:!!')).toBe('identity:7:!!');
    expect(filterValueLabel('source', 'unattributed:7')).toBe('unattributed:7');
  });

  it('labels every filter dimension the API defines', () => {
    for (const dimension of Object.values(FilterDimensions)) {
      const label = filterDimensionLabel(dimension);
      expect(label, dimension).not.toBe('');
      expect(label, dimension).not.toContain('_');
      expect(label, dimension).not.toBe(dimension);
    }
  });

  it('names file types from the MIME type, then the family', () => {
    expect(fileTypeLabel('application/pdf', 'pdf')).toBe('PDF');
    expect(fileTypeLabel('text/tab-separated-values', 'text')).toBe('TSV');
    expect(fileTypeLabel('image/png', 'image')).toBe('PNG image');
    expect(fileTypeLabel('image/webp', 'image')).toBe('WEBP image');
    expect(
      fileTypeLabel(
        'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
        'document'
      )
    ).toBe('Word document');
    expect(fileTypeLabel('', 'archive')).toBe('Archive');
    expect(fileTypeLabel(undefined, undefined)).toBe('Unknown type');
    expect(fileTypeLabel('', 'unrecognized-family')).toBe('Unknown type');
  });

  it('turns every server preflight reason into a sentence', () => {
    const reasons: [string, string, string][] = [
      [
        'export', 'browser_export_requires_single_message',
        'Export works for one message at a time.'
      ],
      [
        'export', 'selection_has_no_exportable_raw_message',
        'The selection has no original message to export.'
      ],
      ['export', 'raw_message_unavailable', 'The original message isn’t available.'],
      ['export_files', 'selection_contains_no_files', 'The selection has no files.'],
      [
        'stage_deletion',
        'selection_contains_items_that_cannot_be_deleted_from_source',
        'None of the selected items can be deleted from their source.'
      ]
    ];
    for (const [action, reason, sentence] of reasons) {
      expect(preflightReasonLabel(action, reason)).toBe(sentence);
    }
    expect(preflightReasonLabel('export', 'some_new_reason')).toBe('Some new reason.');
  });
});

describe('entryKindPresentation', () => {
  it.each([
    ['imessage', 'Conversation'],
    ['sms', 'Conversation'],
    ['whatsapp', 'Conversation'],
    ['slack', 'Conversation'],
    ['inline', 'Conversation'],
    ['chatwoot', 'Conversation'],
    ['facebook_comments', 'Conversation'],
    ['instagram_comments', 'Conversation'],
    ['fbmessenger', 'Conversation'],
    ['matrix', 'Conversation'],
    ['telegram', 'Conversation'],
    ['x_dm', 'Conversation'],
    ['chat', 'Conversation'],
    ['meeting_transcript', 'Meeting'],
    ['meeting_note', 'Meeting'],
    ['meeting_minutes', 'Meeting'],
    ['calendar_event', 'Calendar event'],
    ['email', 'Email'],
    ['', 'Item'],
    ['unheard_of', 'Item']
  ])('names message type %s "%s" when the row has no kind', (messageType, name) => {
    expect(entryKindPresentation('message', messageType).name).toBe(name);
  });

  it('prefers the server kind over the message type', () => {
    expect(entryKindPresentation('meeting', 'email').name).toBe('Meeting');
  });
});

it.each([
 ['month', '2022-05', '2022-05', 'May 2022'],
 ['message_type', 'meeting_transcript', 'meeting_transcript', 'Meeting transcript'],
 ['kind', 'meeting', 'meeting', 'Meeting']
] as const)('formats %s group names', (dimension, key, label, name) => {
 expect(groupRowName(dimension, key, label).name).toBe(name);
});

it('uses the meeting icon for a Granola source group', () => {
  expect(groupRowName('source', '7', 'fixture-granola', new Map([['7', { type: 'granola', identifier: 'fixture-granola' }]])).icon).toBe(Mic);
});

it('disambiguates source labels using the full inventory', () => {
  const sources = new Map([
    ['1', { type: 'imap', identifier: 'imaps://person@example.org@one.example' }],
    ['2', { type: 'imap', identifier: 'imaps://person@example.org@two.example' }],
    ['3', { type: 'mbox', identifier: 'archive.example' }],
    ['4', { type: 'mbox', identifier: 'archive.example' }],
    ['5', { type: 'gcal', identifier: 'work.example' }],
    ['6', { type: 'gcal', identifier: 'home.example' }]
  ]);
  expect(groupRowName('source', '1', sources.get('1')!.identifier, sources).name).toBe('person@example.org (imaps://person@example.org@one.example)');
  expect(groupRowName('source', '2', sources.get('2')!.identifier, sources).name).toBe('person@example.org (imaps://person@example.org@two.example)');
  expect(groupRowName('source', '3', 'archive.example', sources).name).toBe('Mbox import (archive.example · 3)');
  expect(groupRowName('source', '4', 'archive.example', sources).name).toBe('Mbox import (archive.example · 4)');
  expect(groupRowName('source', '5', 'work.example', sources).name).toBe('Google Calendar (work.example)');
  expect(groupRowName('source', '6', 'home.example', sources).name).toBe('Google Calendar (home.example)');
});

it('avoids repeating a mailbox name in colliding source labels', () => {
  const gmailAndImap = new Map([
    ['7', { type: 'gmail', identifier: 'person@example.com' }],
    ['8', { type: 'imap', identifier: 'imaps://person@example.com@mail.example.com' }]
  ]);
  expect(groupRowName('source', '7', 'person@example.com', gmailAndImap).name).toBe('person@example.com (7)');
  expect(groupRowName('source', '8', 'imaps://person@example.com@mail.example.com', gmailAndImap).name).toBe('person@example.com (imaps://person@example.com@mail.example.com)');

  const gmailAndMicrosoft = new Map([
    ['7', { type: 'gmail', identifier: 'person@example.com' }],
    ['9', { type: 'msmail', identifier: 'person@example.com' }]
  ]);
  expect(groupRowName('source', '7', 'person@example.com', gmailAndMicrosoft).name).toBe('person@example.com (7)');
  expect(groupRowName('source', '9', 'person@example.com', gmailAndMicrosoft).name).toBe('person@example.com (9)');
});
