/* Move dex. Category is derived from type (see data/types.js), Gen-3 style.
 *   power/accuracy/pp : as usual (power 0 = status move)
 *   priority : turn-order bonus (default 0); higher goes first
 *   multi    : number of hits in one use
 *   recoil   : fraction of damage dealt taken back by the attacker
 *   heal     : fraction of max HP restored to the user (status move)
 *   effect   : { status, chance } | { stat, stages, target } | { flinch } */
export const MOVES = {
  // --- normal ---
  scratch:  { id: "scratch",  name: "Scratch",   type: "normal", power: 40, accuracy: 100, pp: 35 },
  tackle:   { id: "tackle",   name: "Tackle",    type: "normal", power: 35, accuracy: 95,  pp: 35 },
  growl:    { id: "growl",    name: "Growl",     type: "normal", power: 0,  accuracy: 100, pp: 40, effect: { stat: "atk", stages: -1, target: "foe" } },
  tailwhip: { id: "tailwhip", name: "Tail Whip", type: "normal", power: 0,  accuracy: 100, pp: 30, effect: { stat: "def", stages: -1, target: "foe" } },
  quickstrike: { id: "quickstrike", name: "Quick Strike", type: "normal", power: 40, accuracy: 100, pp: 30, priority: 1 },
  doublehit: { id: "doublehit", name: "Double Hit", type: "normal", power: 25, accuracy: 90, pp: 15, multi: 2 },
  takedown: { id: "takedown", name: "Take Down", type: "normal", power: 70, accuracy: 90, pp: 20, recoil: 0.25 },
  headbutt: { id: "headbutt", name: "Headbutt", type: "normal", power: 45, accuracy: 100, pp: 15, effect: { flinch: 0.3 } },
  recover:  { id: "recover",  name: "Recover",   type: "normal", power: 0, accuracy: 100, pp: 10, heal: 0.5 },
  // --- fire / water / grass ---
  ember:    { id: "ember",    name: "Ember",     type: "fire",  power: 40, accuracy: 100, pp: 25, effect: { status: "burn", chance: 0.1 } },
  flamewheel: { id: "flamewheel", name: "Flame Wheel", type: "fire", power: 60, accuracy: 100, pp: 15, effect: { status: "burn", chance: 0.1 } },
  vinewhip: { id: "vinewhip", name: "Vine Whip", type: "grass", power: 45, accuracy: 100, pp: 25 },
  spore:    { id: "spore",    name: "Spore",     type: "grass", power: 0,  accuracy: 100, pp: 15, effect: { status: "sleep" } },
  stunspore:{ id: "stunspore",name: "Stun Spore",type: "grass", power: 0,  accuracy: 90,  pp: 30, effect: { status: "paralysis" } },
  watergun: { id: "watergun", name: "Water Gun", type: "water", power: 40, accuracy: 100, pp: 25 },
  bubble:   { id: "bubble",   name: "Bubble",    type: "water", power: 40, accuracy: 100, pp: 30, effect: { stat: "speed", stages: -1, target: "foe", chance: 0.1 } },
  // --- electric / rock / ice / psychic / poison ---
  thundershock: { id: "thundershock", name: "Thunder Shock", type: "electric", power: 40, accuracy: 100, pp: 30, effect: { status: "paralysis", chance: 0.1 } },
  spark:    { id: "spark",    name: "Spark",     type: "electric", power: 65, accuracy: 100, pp: 20, effect: { status: "paralysis", chance: 0.3 } },
  rockthrow:{ id: "rockthrow",name: "Rock Throw",type: "rock", power: 50, accuracy: 90, pp: 15 },
  rockslide:{ id: "rockslide",name: "Rock Slide",type: "rock", power: 75, accuracy: 90, pp: 10, effect: { flinch: 0.3 } },
  frostbite:{ id: "frostbite",name: "Frostbite", type: "ice", power: 40, accuracy: 100, pp: 25 },
  icebeam:  { id: "icebeam",  name: "Ice Beam",  type: "ice", power: 70, accuracy: 100, pp: 10 },
  confusion:{ id: "confusion",name: "Confusion", type: "psychic", power: 50, accuracy: 100, pp: 25 },
  psybeam:  { id: "psybeam",  name: "Psybeam",   type: "psychic", power: 65, accuracy: 100, pp: 20 },
  poisonsting: { id: "poisonsting", name: "Poison Sting", type: "poison", power: 30, accuracy: 100, pp: 35, effect: { status: "poison", chance: 0.3 } },
  sludge:   { id: "sludge",   name: "Sludge",    type: "poison", power: 65, accuracy: 100, pp: 20, effect: { status: "poison", chance: 0.3 } },
  // fallback when no PP remains
  struggle: { id: "struggle", name: "Struggle",  type: "normal", power: 50, accuracy: 100, pp: 1 },
};
