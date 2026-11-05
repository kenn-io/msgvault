import { describe, expect, it } from 'vitest';

import { shellQuote } from './shell-quote';
import { withShellArgv } from './shell-argv.test-helper';

const roundTripValues = [
  'you@example.com',
  "o'brien@example.com",
  'Work App',
  'Work & Home',
  'Work $Budget',
  '$(echo LEAKED)',
  'a&b|c<d>e^f(g)',
  'a"b@example.com',
  'say "hi" & leave',
  'Work %QUOTE_PROBE% App',
  '100%',
  'Work!',
  'Hey!QUOTE_PROBE!',
  '!undefined!',
  'a^b%QUOTE_PROBE%',
  '%%!!',
  'x\\"y\\%z!',
  'trailing\\',
  'C:\\Users\\me\\"quoted"\\',
  '',
];

describe('shellQuote', () => {
  it('round-trips arguments through the platform shell', () => {
    withShellArgv((run, shell) => {
      const pairs = [
        ...roundTripValues.map((value) => [value, 'Work & echo LEAKED']),
        ['a^b@example.com', 'Work!'],
        ['a^b@example.com', '!QUOTE_PROBE!'],
        ['a!b@example.com', 'Work ^Budget'],
        ['a!b@example.com', 'Work ^!Budget'],
        ['you@example.com', 'Work $Budget'],
      ];
      for (const [email, app] of pairs) {
        const args = `${shellQuote(email, shell)} --oauth-app ${shellQuote(app, shell)}`;
        expect(run(args), `${email} ${app}`).toEqual([email, '--oauth-app', app]);
      }
    });
  }, 60_000);
});
