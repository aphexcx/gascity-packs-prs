// The off-parity hook (PR 104 round 2, item 11), at the PR's base
// c99694367081b2b3e98a58aafaf1320e091621fe. The base has no arrival service
// and no stopwatch, so the hook names the base and adds nothing to the
// adapter process's environment. Copy it over whatsapp/adapter/test/
// offparity-hook.js in an extracted base tree to re-run the base.

export const tree = 'base';
export function hookEnv() { return {}; }
