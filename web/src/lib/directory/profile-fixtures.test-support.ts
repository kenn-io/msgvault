import type { DirectoryReadBundle } from './models';

export function emptyProfile(person: NonNullable<DirectoryReadBundle['person']>): NonNullable<DirectoryReadBundle['structuredProfile']> {
  return { person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] };
}
