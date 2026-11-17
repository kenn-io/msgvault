import { describe, expect, it } from 'vitest';
import { decisionSentence, evidenceSourceWords, originWords, projectionWords, relationWords, scoreWords } from './fact-words';

describe('fact words', () => {
  it('names evidence sources in words', () => {
    // Unknown values preserve safe copy if a newer server adds an enum value.
    expect(evidenceSourceWords('unknown', 'direct-self')).toBe('');
    expect(evidenceSourceWords('archive', 'direct-self')).toBe('From a message they sent');
    expect(evidenceSourceWords('archive', 'direct-other')).toBe('From a message or meeting');
    expect(evidenceSourceWords('archive', 'indirect')).toBe('Inferred from your messages');
    expect(evidenceSourceWords('public', 'indirect')).toBe('From a public page');
    expect(evidenceSourceWords('provider_assertion', 'indirect')).toBe('From a lookup');
    expect(evidenceSourceWords('system', 'direct-self')).toBe('Recorded automatically');
  });

  it('says only what departs from normal for claims', () => {
    expect(relationWords('support')).toBeNull();
    expect(relationWords('contradict')).toBe('Says this value isn’t true');
    expect(relationWords('supersede')).toBe('Reports a replacement');
    expect(relationWords('invalid')).toBe('Not usable');
    expect(originWords('system')).toBe('msgvault');
    expect(originWords('enrichment')).toBe('From a lookup');
    // Unknown values preserve safe copy if a newer server adds an enum value.
    expect(originWords('unknown')).toBe('');
    expect(relationWords('unknown')).toBeNull();
  });

  it('words recorded outcomes and reasons', () => {
    expect(decisionSentence('saved', '', 'New York')).toBe('Saved: New York');
    expect(decisionSentence('value-retired', 'explicit-supersession', 'New York')).toBe('This value is no longer saved: New York');
    expect(decisionSentence('current-value-kept', 'insufficient-margin', 'New York')).toBe("Kept the current value; New York wasn't far enough ahead");
    expect(decisionSentence('current-value-kept', 'explicit-contradiction', 'New York')).toBe('New York: Kept the current value');
    expect(decisionSentence('pinned-value-kept', 'pin-retained', 'New York')).toBe('New York: Kept your pinned value');
    expect(decisionSentence('too-close-to-choose', 'competing-tie', 'New York')).toBe('The evidence was too close to choose: New York');
    expect(decisionSentence('not-used', 'sensitive-policy', 'New York')).toBe('Not used: New York · this field is sensitive');
    expect(decisionSentence('not-used', 'applied-projection', 'New York')).toBe('Not used: New York · another claim was saved');
    expect(decisionSentence('not-used', 'insufficient-margin', 'New York')).toBe('Not used: New York');
    expect(decisionSentence('not-used', 'identity-mismatch', 'New York')).toBe('Not used: New York · may be about someone else');
    expect(decisionSentence('saved', '', '')).toBe('Result unavailable');
    expect(decisionSentence('current-value-kept', 'insufficient-margin')).toBe('Result unavailable');
    expect(decisionSentence('not-used', 'sensitive-policy')).toBe('Result unavailable');
    expect(decisionSentence(null, '', 'New York')).toBe('Result unavailable');
  });

  it('names where a saved decision put its value', () => {
    expect(projectionWords('person_attribute')).toBeNull();
    expect(projectionWords('employment')).toBe('Work');
  });

  it('shows recorded scores without assigning strength', () => {
    expect(scoreWords(850)).toBe('850 of 1000');
  });

});
