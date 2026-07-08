/* Stat, experience, and leveling math — the authentic Gen-3 (FireRed) formulas
 * with IV = EV = 0 and a neutral nature. Pure module (no DOM), so it can be
 * unit-tested directly in Node. */
import { CONFIG } from "../data/config.js";

// HP    = floor(2*Base * Level / 100) + Level + 10
// Other = floor(2*Base * Level / 100) + 5
export const calcStat = (base, level, isHP) =>
  Math.floor((2 * base * level) / 100) + (isHP ? level + 10 : 5);

// Medium-fast growth group: total EXP to reach a level = level^3.
export const expForLevel = (level) => level * level * level;
export const levelForExp = (exp) => Math.max(1, Math.floor(Math.cbrt(exp)));

export function recomputeStats(cr, keepHpDelta) {
  const b = cr.base, prevMax = cr.maxhp;
  cr.maxhp = calcStat(b.hp, cr.level, true);
  cr.atk = calcStat(b.atk, cr.level, false);
  cr.def = calcStat(b.def, cr.level, false);
  cr.spAtk = calcStat(b.spAtk, cr.level, false);
  cr.spDef = calcStat(b.spDef, cr.level, false);
  cr.speed = calcStat(b.speed, cr.level, false);
  if (keepHpDelta) cr.hp = Math.min(cr.maxhp, cr.hp + (cr.maxhp - prevMax));
}

// Adds EXP, leveling up as thresholds are crossed. Returns the new levels hit.
export function gainExp(cr, amount) {
  cr.exp += amount;
  const gained = [];
  while (cr.level < CONFIG.battle.maxLevel && cr.exp >= expForLevel(cr.level + 1)) {
    cr.level++;
    recomputeStats(cr, true);
    gained.push(cr.level);
  }
  return gained;
}

// EXP awarded for defeating `foe` (single-battler form).
export const expYield = (foe) =>
  Math.max(1, Math.floor((foe.expYield * foe.level) / CONFIG.battle.expDivisor));
