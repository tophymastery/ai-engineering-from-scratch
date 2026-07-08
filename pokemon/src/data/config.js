/* Global game configuration. Tune the whole game from this one file.
 * All values are plain data — safe to edit without touching engine code. */
export const CONFIG = {
  grid: {
    tile: 16,     // logical tile size in px
    scale: 2,     // on-screen zoom (draw size = tile * scale = 32px)
  },
  screen: {
    width: 704,    // canvas width  (must match <canvas> in index.html)
    height: 480,   // canvas height
    sidebar: 160,  // right-hand overworld panel (badges / party)
  },
  movement: {
    moveFrames: 12,   // frames to walk one tile (lower = faster)
  },
  starter: {
    species: "emberling",   // id from data/species.js — the hardcoded first partner
    level: 5,
  },
  party: { max: 6 },        // creatures carried at once; extras go to storage
  economy: {
    startMoney: 1500,       // wallet on a new game
    trainerPrizePerLevel: 40,  // trainer prize = lead foe level * this
    gymPrize: 800,          // flat prize for clearing a gym
  },
  // Battle mechanics. These mirror the FireRed (Gen-3) engine and are tunable.
  battle: {
    critChance: 1 / 16,   // Gen-3 base critical-hit rate
    critMult: 2,          // critical-hit damage multiplier
    stab: 1.5,            // same-type attack bonus
    randMin: 85,          // damage random factor lower bound (percent)
    randMax: 100,         // damage random factor upper bound (percent)
    expDivisor: 7,        // EXP yield = floor(baseYield * foeLevel / expDivisor)
    maxLevel: 100,
    // Status-condition effects (fraction of max HP per turn, and speed cut).
    burnDamage: 1 / 8, poisonDamage: 1 / 8, paralysisSpeed: 0.5,
    paralysisSkipChance: 0.25, sleepMin: 1, sleepMax: 3,
    burnAtkMult: 0.5,     // burn halves physical Attack
  },
  catch: {
    // Simplified Gen-style catch chance: higher when the foe's HP is low.
    // p = clamp( ballBonus * (maxHp*3 - hp*2) / (maxHp*3) , 0..1 )  * statusBonus
    statusBonus: { none: 1.0, sleep: 2.0, paralysis: 1.5, poison: 1.2, burn: 1.2 },
  },
  // Battle animation pacing (in frames @ ~60fps).
  animation: {
    hpFrames: 24,     // how quickly HP bars drain to their target
    lungeFrames: 10,  // attacker lunge duration
    flashFrames: 16,  // defender hit-flash duration
    faintFrames: 30,  // faint slide/fade duration
  },
  badges: { total: 8 },   // sidebar badge slots (1st = Leaf Badge from the gym)
};
