/* Factories that turn dex data into live battle instances. Pure module. */
import { SPECIES } from "../data/species.js";
import { MOVES } from "../data/moves.js";
import { CONFIG } from "../data/config.js";
import { recomputeStats, expForLevel } from "./stats.js";

export const makeMove = (id) => {
  const m = MOVES[id];
  return { ...m, pp: m.pp, maxpp: m.pp };
};

// The (up to 4) most recently learnable moves at a given level.
export function movesAtLevel(species, level) {
  const known = species.learnset.filter((l) => l.level <= level).map((l) => l.move);
  const unique = [...new Set(known)];
  const slice = unique.slice(-4);
  return slice.length ? slice : [species.learnset[0].move];
}

export function makeCreature(id, level) {
  const s = SPECIES[id];
  if (!s) throw new Error(`Unknown species: ${id}`);
  const cr = {
    speciesId: id, name: s.name, type: s.type,
    sprite: s.sprite, base: s.base, expYield: s.expYield,
    level, exp: expForLevel(level),
    status: "none", sleepTurns: 0, flinched: false,
    stages: { atk: 0, def: 0, spAtk: 0, spDef: 0, speed: 0 },
    moves: movesAtLevel(s, level).map(makeMove),
  };
  recomputeStats(cr, false);
  cr.hp = cr.maxhp;
  return cr;
}

// Build a party (array of creatures) from a [{species, level}] list.
export const makeParty = (list) => list.map((e) => makeCreature(e.species, e.level));

// Reset volatile battle state (stat stages) when a creature is withdrawn.
export function resetStages(cr) { cr.stages = { atk: 0, def: 0, spAtk: 0, spDef: 0, speed: 0 }; }

// Moves this species learns exactly at `level`.
export const learnableAt = (cr, level) =>
  SPECIES[cr.speciesId].learnset.filter((l) => l.level === level).map((l) => l.move);

// Teach a move if there's a free slot. Returns "learned" | "full" | "known".
export function learnMove(cr, moveId) {
  if (cr.moves.some((m) => m.id === moveId)) return "known";
  if (cr.moves.length >= 4) return "full";
  cr.moves.push(makeMove(moveId));
  return "learned";
}

// Evolve in place if the level threshold is met. Returns the new name or null.
export function evolveIfReady(cr) {
  const evo = SPECIES[cr.speciesId].evolvesTo;
  if (!evo || cr.level < evo.level) return null;
  const into = SPECIES[evo.into];
  cr.speciesId = evo.into; cr.name = into.name; cr.type = into.type;
  cr.sprite = into.sprite; cr.base = into.base; cr.expYield = into.expYield;
  recomputeStats(cr, true);
  return into.name;
}
