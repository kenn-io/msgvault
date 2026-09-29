import { describe, expect, it } from 'vitest';

import { ExploreFilterDimension as FilterDimensions } from '../api/generated/models/exploreFilterDimension';
import {
  fileTypeLabel,
  filterDimensionLabel,
  preflightReasonLabel,
  presentationLabel,
  searchModeLabel
} from './labels';

describe('explore labels', () => {
  it('names search modes and presentations', () => {
    expect(searchModeLabel('full_text')).toBe('Full text');
    expect(searchModeLabel('semantic')).toBe('Semantic');
    expect(searchModeLabel('hybrid')).toBe('Hybrid');
    expect(presentationLabel('timeline')).toBe('Timeline');
  });

  it('names filter dimensions as chips and falls back to sentence case', () => {
    expect(filterDimensionLabel('source')).toBe('Source');
    expect(filterDimensionLabel('participant')).toBe('Person');
    expect(filterDimensionLabel('domain')).toBe('Domain');
    expect(filterDimensionLabel('message_type')).toBe('Message type');
    expect(filterDimensionLabel('mailing_list')).toBe('Mailing list');
    expect(filterDimensionLabel('made_up_dimension' as never)).toBe('Made up dimension');
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
    expect(fileTypeLabel('image/png', 'image')).toBe('PNG image');
    expect(fileTypeLabel('image/webp', 'image')).toBe('WEBP image');
    expect(
      fileTypeLabel('application/vnd.openxmlformats-officedocument.wordprocessingml.document', 'document')
    ).toBe('Word document');
    expect(fileTypeLabel('', 'archive')).toBe('Archive');
    expect(fileTypeLabel(undefined, undefined)).toBe('Unknown type');
  });

  it('turns every server preflight reason into a sentence', () => {
    const reasons: [string, string, string][] = [
      ['open_in_source', 'trusted_source_link_unavailable', 'Your sources don’t provide links to open these items.'],
      ['export', 'browser_export_requires_single_message', 'Export works for one message at a time.'],
      ['export', 'selection_has_no_exportable_raw_message', 'The selection has no original message to export.'],
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
