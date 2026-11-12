import { describe, expect, it } from 'vitest';
import { authorityWords, decisionSentence, evidenceSourceWords, lostSupportWords, originWords, projectionWords, relationWords, scoreWords, statusEventWords } from './fact-words';

describe('fact words', () => {
  it('names evidence sources in words', () => {
    expect(evidenceSourceWords('unknown', 'direct-self')).toBe('');
    expect(evidenceSourceWords('archive', 'direct-self')).toBe('From a message they sent');
    expect(evidenceSourceWords('archive', 'direct-other')).toBe('From a message about them');
    expect(evidenceSourceWords('archive', 'indirect')).toBe('Inferred from your messages');
    expect(evidenceSourceWords('public', 'indirect')).toBe('From a public page');
    expect(evidenceSourceWords('provider_assertion', 'indirect')).toBe('From a lookup');
    expect(evidenceSourceWords('system', 'direct-self')).toBe('Recorded automatically');
    expect(authorityWords('ordinary')).toBeNull();
    expect(authorityWords('authoritative')).toBe('official record');
  });

  it('says only what departs from normal for claims', () => {
    expect(relationWords('support')).toBeNull();
    expect(relationWords('contradict')).toBe('Says this value isn’t true');
    expect(relationWords('supersede')).toBe('Reports a replacement');
    expect(relationWords('invalid')).toBe('Not usable');
    expect(originWords('system')).toBe('msgvault');
    expect(originWords('enrichment')).toBe('From a lookup');
    expect(originWords('unknown')).toBe('');
    expect(relationWords('unknown')).toBeNull();
  });

  it('words recorded outcomes and reasons', () => {
    expect(decisionSentence('saved', '', 'New York')).toBe('Saved: New York');
    expect(decisionSentence('saved', '')).toBe('Saved');
    expect(decisionSentence('value-retired', '')).toBe('This value is no longer saved');
    expect(decisionSentence('current-value-kept', 'insufficient-margin', 'New York')).toBe("Kept the current value; New York wasn't far enough ahead");
    expect(decisionSentence('current-value-kept', 'insufficient-margin')).toBe("Kept the current value; the claim wasn't far enough ahead");
    expect(decisionSentence('current-value-kept', 'explicit-contradiction', 'New York')).toBe('Kept the current value');
    expect(decisionSentence('pinned-value-kept', '')).toBe('Kept your pinned value');
    expect(decisionSentence('too-close-to-choose', '')).toBe('The evidence was too close to choose');
    expect(decisionSentence('not-used', 'sensitive-policy')).toBe('Not used: this field is sensitive');
    expect(decisionSentence('not-used', 'identity-mismatch')).toBe('Not used: may be about someone else');
    expect(decisionSentence(null, '')).toBe('Result unavailable');
  });

  it('words source status changes', () => {
    expect(statusEventWords(true, 'source-reimported')).toBe('Imported again');
    expect(statusEventWords(true, 'scope-relinked')).toBe('Linked again');
    expect(statusEventWords(false, 'scope-unlinked')).toBe('Unlinked from this person');
    expect(statusEventWords(false, 'source-deleted')).toBe('Source deleted');
    expect(statusEventWords(false, 'source-edited')).toBe('Source edited');
    expect(lostSupportWords('identity-reassigned')).toBe('Moved to another person');
    expect(lostSupportWords(undefined)).toBe('Source removed');
  });

  it('names where a saved decision put its value', () => {
    expect(projectionWords('person_attribute')).toBe('Field');
  });

  it('names confidence strength at the 600 and 800 boundaries', () => {
    expect(scoreWords(599)).toBe('Weak · 599 of 1000');
    expect(scoreWords(600)).toBe('Moderate · 600 of 1000');
    expect(scoreWords(799)).toBe('Moderate · 799 of 1000');
    expect(scoreWords(800)).toBe('Strong · 800 of 1000');
  });

});
