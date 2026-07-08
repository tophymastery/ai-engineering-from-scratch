/* Pure-logic test suite — runs in Node (no browser). Covers formulas, data
 * completeness/consistency, map integrity, and a simulated battle. */
import { CONFIG } from "../src/data/config.js";
import { TYPE_CHART, TYPE_COLORS, typeEffectiveness, isSpecial } from "../src/data/types.js";
import { MOVES } from "../src/data/moves.js";
import { SPECIES } from "../src/data/species.js";
import { ITEMS, STARTING_BAG } from "../src/data/items.js";
import { ENCOUNTERS } from "../src/data/encounters.js";
import { MAPS, WARPS, NPCS, DOORS, isBlocked } from "../src/data/maps.js";
import { calcStat, expForLevel, levelForExp, gainExp, expYield } from "../src/engine/stats.js";
import { calcDamage } from "../src/engine/damage.js";
import { makeCreature } from "../src/engine/creature.js";
import { setRng } from "../src/core/rng.js";
import { player, game, battle, STATE } from "../src/state.js";
import { startWildBattle, battleInput } from "../src/engine/battle.js";

let pass = 0, fail = 0;
const ok = (name, cond) => { if (cond) { pass++; } else { fail++; console.error("  FAIL:", name); } };
const eq = (name, a, b) => ok(`${name} (got ${a}, want ${b})`, a === b);
const section = (s) => console.log(`\n# ${s}`);

// ---------------------------------------------------------------- types
section("type chart");
eq("fire>grass", typeEffectiveness("fire", "grass"), 2.0);
eq("fire<water", typeEffectiveness("fire", "water"), 0.5);
eq("water>fire", typeEffectiveness("water", "fire"), 2.0);
eq("grass>water", typeEffectiveness("grass", "water"), 2.0);
eq("normal=fire", typeEffectiveness("normal", "fire"), 1.0);
ok("normal is physical", !isSpecial("normal"));
ok("fire is special", isSpecial("fire"));

// ---------------------------------------------------------------- stat formula
section("stat formula (Gen-3, IV=EV=0)");
// HP = floor(2*Base*Level/100) + Level + 10 ; Other = floor(2*Base*Level/100) + 5
eq("HP base45 L5", calcStat(45, 5, true), Math.floor((2 * 45 * 5) / 100) + 5 + 10);
eq("ATK base52 L5", calcStat(52, 5, false), Math.floor((2 * 52 * 5) / 100) + 5);
eq("HP base60 L50", calcStat(60, 50, true), Math.floor((2 * 60 * 50) / 100) + 50 + 10);

// ---------------------------------------------------------------- exp curve
section("experience curve (medium-fast, level^3)");
eq("expForLevel(6)", expForLevel(6), 216);
eq("expForLevel(10)", expForLevel(10), 1000);
eq("levelForExp(216)", levelForExp(216), 6);
{
  const c = makeCreature("emberling", 5);
  const beforeHp = c.maxhp;
  c.exp = expForLevel(6) - 1;
  const levels = gainExp(c, 5);
  ok("gainExp levels up", levels.includes(6) && c.level === 6);
  ok("level-up raises maxHP", c.maxhp > beforeHp);
}

// ---------------------------------------------------------------- damage formula
section("damage formula (FireRed)");
{
  const atk = makeCreature("emberling", 10);   // fire
  const grassDef = makeCreature("wormling", 10);
  const normalDef = makeCreature("nibbit", 10);
  const ember = MOVES.ember;
  const superEff = calcDamage(atk, grassDef, ember, { forceCrit: false, rand: 100 }).dmg;
  const neutral = calcDamage(atk, normalDef, ember, { forceCrit: false, rand: 100 }).dmg;
  ok("super-effective > neutral", superEff > neutral);
  const res = calcDamage(atk, grassDef, ember, { forceCrit: false, rand: 100 });
  ok("super-effective flagged", res.eff === 2.0);
  const crit = calcDamage(atk, normalDef, ember, { forceCrit: true, rand: 100 }).dmg;
  const noCrit = calcDamage(atk, normalDef, ember, { forceCrit: false, rand: 100 }).dmg;
  ok("crit deals more", crit > noCrit);
  ok("min-1 damage rule", calcDamage(makeCreature("nibbit", 2), makeCreature("cavvit", 30), MOVES.tackle, { rand: 85 }).dmg >= 1);
}

// ---------------------------------------------------------------- data completeness
section("data completeness & consistency");
for (const [id, s] of Object.entries(SPECIES)) {
  ok(`${id} type in chart`, !!TYPE_CHART[s.type]);
  ok(`${id} has sprite`, !!s.sprite && !!s.sprite.shape && !!s.sprite.color);
  ok(`${id} has 6 base stats`, ["hp", "atk", "def", "spAtk", "spDef", "speed"].every((k) => typeof s.base[k] === "number"));
  ok(`${id} expYield`, typeof s.expYield === "number" && s.expYield > 0);
  ok(`${id} moves valid`, s.moves.length > 0 && s.moves.every((m) => !!MOVES[m]));
}
for (const [id, m] of Object.entries(MOVES)) {
  ok(`move ${id} type has color`, !!TYPE_COLORS[m.type]);
  ok(`move ${id} fields`, typeof m.power === "number" && typeof m.pp === "number" && typeof m.accuracy === "number");
}
for (const [id, it] of Object.entries(ITEMS)) ok(`item ${id} fields`, !!it.kind && typeof it.value === "number");
ok("starting bag items valid", STARTING_BAG.every((b) => !!ITEMS[b.item]));
ok("starter species exists", !!SPECIES[CONFIG.starter.species]);
for (const [area, e] of Object.entries(ENCOUNTERS)) {
  ok(`encounter ${area} rate`, e.rate > 0 && e.rate <= 1);
  ok(`encounter ${area} species valid`, e.table.every((t) => !!SPECIES[t.species] && t.min <= t.max));
}
for (const npc of NPCS.town.filter((n) => n.role === "trainer"))
  ok(`trainer ${npc.name} party valid`, npc.party.every((p) => !!SPECIES[p.species]));

// ---------------------------------------------------------------- map integrity
section("map integrity + reachability");
for (const [name, m] of Object.entries(MAPS)) {
  ok(`${name} rectangular`, m.grid.every((r) => r.length === m.w));
}
for (const [key, w] of Object.entries(WARPS)) {
  ok(`warp ${key} -> valid map`, !!MAPS[w.map]);
  ok(`warp ${key} -> walkable`, !isBlocked(w.map, w.x, w.y));
}
// BFS from the tile just outside Home; every building door must be reachable.
{
  const start = { x: DOORS.H.x, y: DOORS.H.y + 1 };
  const seen = new Set([`${start.x},${start.y}`]);
  const q = [start];
  while (q.length) {
    const { x, y } = q.shift();
    for (const [dx, dy] of [[0, -1], [0, 1], [-1, 0], [1, 0]]) {
      const nx = x + dx, ny = y + dy, k = `${nx},${ny}`;
      if (seen.has(k) || isBlocked("town", nx, ny)) continue;
      seen.add(k); q.push({ x: nx, y: ny });
    }
  }
  for (const ch of ["G", "L", "C", "V", "E"])
    ok(`door ${ch} reachable`, seen.has(`${DOORS[ch].x},${DOORS[ch].y}`));
}

// ---------------------------------------------------------------- simulated battle
section("simulated battle: found -> win + level up");
{
  setRng(() => 0.99);                    // max damage, no crit, deterministic
  player.party = [makeCreature("emberling", 5)];
  player.party[0].exp = expForLevel(6) - 1;  // one win will cross into level 6
  game.state = STATE.WORLD;
  startWildBattle(ENCOUNTERS.town);
  ok("battle entered", game.state === STATE.BATTLE);
  let guard = 0;
  while (game.state === STATE.BATTLE && guard++ < 400) {
    if (battle.phase === "anim") battleInput("action");
    else if (battle.phase === "menu") battleInput("action");   // FIGHT
    else if (battle.phase === "moves") battleInput("action");  // first move
    else battleInput("action");
  }
  eq("returned to overworld", game.state, STATE.WORLD);
  ok("ally leveled up from the win", player.party[0].level >= 6);
  setRng(null);
}

// ---------------------------------------------------------------- summary
console.log(`\n=== logic: ${pass} passed, ${fail} failed ===`);
process.exit(fail ? 1 : 0);
