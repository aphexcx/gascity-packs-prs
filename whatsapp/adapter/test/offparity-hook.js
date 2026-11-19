// The off-parity hook (PR 104 round 2, item 11), at the head and at the tip.
// The one place where offparity.test.js may reach wiring newer than the PR's
// base. On WhatsApp the adapter process wires the arrival service and the
// stopwatch itself (src/index.js main: createChoice, createStopwatch and
// createArrival before the journal), so the hook adds nothing to the
// process's environment. The base's copy of this file names the base and adds
// nothing either: the base has neither.

export const tree = 'head or tip';
export function hookEnv() { return {}; }
