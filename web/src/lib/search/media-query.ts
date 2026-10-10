export function admitsMediaQuery(query: string): boolean {
  return query.trim().split(/\s+/u).every(term => /^[\p{L}\p{M}\p{N}]+$/u.test(term) && /[\p{L}\p{N}]/u.test(term));
}
