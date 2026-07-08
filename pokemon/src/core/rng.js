/* Central random source. Overridable so tests can make battles deterministic. */
let _rng = Math.random;
export const setRng = (fn) => { _rng = fn || Math.random; };
export const rand = () => _rng();
export const rint = (n) => Math.floor(_rng() * n);
// Inclusive integer in [lo, hi]
export const rrange = (lo, hi) => lo + Math.floor(_rng() * (hi - lo + 1));
