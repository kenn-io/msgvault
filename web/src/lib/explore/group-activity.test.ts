import { expect, it } from 'vitest';
import { bucketSeries } from './group-activity';

it.each([
  ['month', [{ bucket: '2024-01', count: 1 }, { bucket: '2024-03', count: 2 }], [{ bucket: '2024-01', count: 1 }, { bucket: '2024-02', count: 0 }, { bucket: '2024-03', count: 2 }]],
  ['day', [{ bucket: '2024-02-28', count: 1 }, { bucket: '2024-03-01', count: 2 }], [{ bucket: '2024-02-28', count: 1 }, { bucket: '2024-02-29', count: 0 }, { bucket: '2024-03-01', count: 2 }]],
])('fills %s gaps oldest first', (_, input, want) => { expect(bucketSeries(input)).toEqual(want); });
