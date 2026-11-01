import { describe, expect, it } from 'vitest';

describe('browser storage test double', () => {
  it('lists a stored key from Object.keys and drops it on remove', () => {
    localStorage.setItem('msgvault.example.claim', '1');
    expect(Object.keys(localStorage)).toEqual(['msgvault.example.claim']);
    expect(localStorage.getItem('msgvault.example.claim')).toBe('1');
    localStorage.removeItem('msgvault.example.claim');
    expect(Object.keys(localStorage)).toEqual([]);
    expect(localStorage.getItem('msgvault.example.claim')).toBeNull();
  });
});
