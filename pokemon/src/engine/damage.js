/* Damage & critical-hit calculation — the authentic Gen-3 (FireRed) formula.
 * Pure module (uses the injectable RNG), so it is directly unit-testable. */
import { CONFIG } from "../data/config.js";
import { typeEffectiveness, isSpecial } from "../data/types.js";
import { rand, rrange } from "../core/rng.js";

export const critChance = () => CONFIG.battle.critChance;

/* opts: { forceCrit?: boolean, rand?: number(85..100) } for deterministic tests */
export function calcDamage(attacker, defender, move, opts = {}) {
  const B = CONFIG.battle;
  const crit = opts.forceCrit != null ? opts.forceCrit : rand() < B.critChance;
  const special = isSpecial(move.type);
  const A = special ? attacker.spAtk : attacker.atk;
  const Dstat = special ? defender.spDef : defender.def;

  // base = floor(floor(floor(2*L/5 + 2) * Power * A / D) / 50) + 2
  let dmg = Math.floor(
    Math.floor(Math.floor((2 * attacker.level) / 5 + 2) * move.power * A / Dstat) / 50
  ) + 2;

  if (crit) dmg = Math.floor(dmg * B.critMult);
  if (move.type === attacker.type) dmg = Math.floor(dmg * B.stab);   // STAB
  const eff = typeEffectiveness(move.type, defender.type);
  dmg = Math.floor(dmg * eff);

  const r = opts.rand != null ? opts.rand : rrange(B.randMin, B.randMax);
  if (dmg > 0) dmg = Math.floor((dmg * r) / 100);
  if (eff > 0 && move.power > 0) dmg = Math.max(1, dmg);   // min-1 rule

  return { dmg, eff, crit, special };
}
