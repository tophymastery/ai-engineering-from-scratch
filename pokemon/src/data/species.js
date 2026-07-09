/* Creature dex — original creatures using the FireRed base-stat schema.
 *   base      : { hp, atk, def, spAtk, spDef, speed }  (Gen-3 six-stat layout)
 *   sprite    : { shape, color }  — sprites are simple geometric shapes
 *   expYield  : base experience awarded when this creature is defeated
 *   learnset  : [{ level, move }] — moves are learned as levels are reached;
 *               a fresh creature knows the (up to 4) most recent ones
 *   evolvesTo : optional { level, into } — evolves on reaching that level
 *
 * Edit numbers/ids here to retune; the engine reads these directly. */
export const SPECIES = {
  emberling: {
    name: "Emberling", type: "fire",
    sprite: { shape: "flame", color: "#f0862c" },
    base: { hp: 45, atk: 52, def: 43, spAtk: 60, spDef: 50, speed: 65 },
    expYield: 62,
    learnset: [{ level: 1, move: "scratch" }, { level: 1, move: "growl" }, { level: 7, move: "ember" }, { level: 13, move: "flamewheel" }],
    evolvesTo: { level: 16, into: "blazehound" },
  },
  blazehound: {
    name: "Blazehound", type: "fire",
    sprite: { shape: "flame", color: "#e2531f" },
    base: { hp: 58, atk: 64, def: 58, spAtk: 80, spDef: 65, speed: 80 },
    expYield: 142,
    learnset: [{ level: 1, move: "scratch" }, { level: 1, move: "ember" }, { level: 16, move: "flamewheel" }],
  },
  wormling: {
    name: "Wormling", type: "grass",
    sprite: { shape: "blob", color: "#7bd06a" },
    base: { hp: 50, atk: 45, def: 55, spAtk: 49, spDef: 65, speed: 35 },
    expYield: 50,
    learnset: [{ level: 1, move: "tackle" }, { level: 4, move: "growl" }, { level: 7, move: "vinewhip" }, { level: 10, move: "stunspore" }],
    evolvesTo: { level: 14, into: "bloomworm" },
  },
  bloomworm: {
    name: "Bloomworm", type: "grass",
    sprite: { shape: "spike", color: "#57b84d" },
    base: { hp: 70, atk: 62, def: 75, spAtk: 70, spDef: 85, speed: 45 },
    expYield: 130,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "vinewhip" }, { level: 14, move: "spore" }],
  },
  nibbit: {
    name: "Nibbit", type: "normal",
    sprite: { shape: "round", color: "#c9a06a" },
    base: { hp: 40, atk: 50, def: 38, spAtk: 30, spDef: 32, speed: 58 },
    expYield: 48,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "tailwhip" }, { level: 5, move: "scratch" }],
  },
  cavvit: {
    name: "Cavvit", type: "normal",
    sprite: { shape: "round", color: "#8a8f9c" },
    base: { hp: 55, atk: 55, def: 70, spAtk: 30, spDef: 40, speed: 25 },
    expYield: 55,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "scratch" }, { level: 8, move: "growl" }],
  },
  dribblet: {
    name: "Dribblet", type: "water",
    sprite: { shape: "blob", color: "#3f9fff" },
    base: { hp: 50, atk: 48, def: 50, spAtk: 55, spDef: 55, speed: 52 },
    expYield: 60,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "watergun" }, { level: 9, move: "bubble" }],
    evolvesTo: { level: 18, into: "torrentyl" },
  },
  torrentyl: {
    name: "Torrentyl", type: "water",
    sprite: { shape: "spike", color: "#2f7fe0" },
    base: { hp: 75, atk: 75, def: 72, spAtk: 85, spDef: 80, speed: 70 },
    expYield: 150,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "watergun" }, { level: 18, move: "bubble" }],
  },
  thornbud: {
    name: "Thornbud", type: "grass",
    sprite: { shape: "spike", color: "#3fae5a" },
    base: { hp: 55, atk: 50, def: 55, spAtk: 62, spDef: 60, speed: 42 },
    expYield: 70,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "vinewhip" }, { level: 1, move: "stunspore" }],
  },
  // --- gym-4-to-8 typed creatures ---
  voltling: {
    name: "Voltling", type: "electric",
    sprite: { shape: "spike", color: "#f7d02c" },
    base: { hp: 45, atk: 50, def: 40, spAtk: 70, spDef: 50, speed: 78 },
    expYield: 90,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "thundershock" }, { level: 12, move: "spark" }, { level: 20, move: "quickstrike" }],
  },
  pebblo: {
    name: "Pebblo", type: "rock",
    sprite: { shape: "round", color: "#b8a038" },
    base: { hp: 60, atk: 72, def: 95, spAtk: 30, spDef: 40, speed: 25 },
    expYield: 95,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "rockthrow" }, { level: 16, move: "rockslide" }, { level: 20, move: "headbutt" }],
  },
  frostpup: {
    name: "Frostpup", type: "ice",
    sprite: { shape: "blob", color: "#8fd6e0" },
    base: { hp: 55, atk: 55, def: 52, spAtk: 65, spDef: 58, speed: 55 },
    expYield: 100,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "frostbite" }, { level: 18, move: "icebeam" }, { level: 22, move: "headbutt" }],
  },
  mindly: {
    name: "Mindly", type: "psychic",
    sprite: { shape: "round", color: "#f85888" },
    base: { hp: 55, atk: 40, def: 48, spAtk: 82, spDef: 74, speed: 64 },
    expYield: 110,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "confusion" }, { level: 20, move: "psybeam" }, { level: 24, move: "recover" }],
  },
  venuff: {
    name: "Venuff", type: "poison",
    sprite: { shape: "blob", color: "#a86fd0" },
    base: { hp: 65, atk: 68, def: 62, spAtk: 55, spDef: 64, speed: 48 },
    expYield: 115,
    learnset: [{ level: 1, move: "tackle" }, { level: 1, move: "poisonsting" }, { level: 18, move: "sludge" }, { level: 24, move: "takedown" }],
  },
};
