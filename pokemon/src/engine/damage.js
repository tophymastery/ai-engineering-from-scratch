/* Damage & critical-hit calculation — the authentic Gen-3 (FireRed) formula.
 * Pure module (uses the injectable RNG), so it is directly unit-testable. */
import { CONFIG } from "../data/config.js";
import { typeEffectiveness, isSpecial } from "../data/types.js";
import { rand, rrange } from "../core/rng.js";

export const critChance = () => CONFIG.battle.critChance;

// Gen-3 stat-stage multiplier: +n -> (2+n)/2, -n -> 2/(2+n).
export const stageMult = (stage) => (stage >= 0 ? (2 + stage) / 2 : 2 / (2 - stage));

/* opts: { forceCrit?: boolean, rand?: number(85..100) } for deterministic tests */
export function calcDamage(attacker, defender, move, opts = {}) {
  const B = CONFIG.battle;
  const crit = opts.forceCrit != null ? opts.forceCrit : rand() < B.critChance;
  const special = isSpecial(move.type);
  const st = (cr, k) => (cr.stages ? stageMult(cr.stages[k]) : 1);
  let A = (special ? attacker.spAtk : attacker.atk) * st(attacker, special ? "spAtk" : "atk");
  const Dstat = (special ? defender.spDef : defender.def) * st(defender, special ? "spDef" : "def");
  if (!special && attacker.status === "burn") A *= B.burnAtkMult;   // burn halves Attack
  A = Math.max(1, Math.floor(A));
  const D = Math.max(1, Math.floor(Dstat));

  // base = floor(floor(floor(2*L/5 + 2) * Power * A / D) / 50) + 2
  let dmg = Math.floor(
    Math.floor(Math.floor((2 * attacker.level) / 5 + 2) * move.power * A / D) / 50
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
