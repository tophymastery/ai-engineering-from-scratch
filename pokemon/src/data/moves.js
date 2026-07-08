/* Move dex. Category is derived from type (see data/types.js), Gen-3 style.
 *   power    : base power (0 = status move)
 *   accuracy : hit chance percent
 *   pp       : uses before exhausted
 *   effect   : optional secondary effect, one of:
 *     { status: "burn"|"poison"|"paralysis"|"sleep", chance }  (chance omitted = always)
 *     { stat: "atk"|"def"|"spAtk"|"spDef"|"speed", stages, target: "foe"|"self" } */
export const MOVES = {
  scratch:  { id: "scratch",  name: "Scratch",   type: "normal", power: 40, accuracy: 100, pp: 35 },
  tackle:   { id: "tackle",   name: "Tackle",    type: "normal", power: 35, accuracy: 95,  pp: 35 },
  growl:    { id: "growl",    name: "Growl",     type: "normal", power: 0,  accuracy: 100, pp: 40, effect: { stat: "atk", stages: -1, target: "foe" } },
  tailwhip: { id: "tailwhip", name: "Tail Whip", type: "normal", power: 0,  accuracy: 100, pp: 30, effect: { stat: "def", stages: -1, target: "foe" } },
  ember:    { id: "ember",    name: "Ember",     type: "fire",   power: 40, accuracy: 100, pp: 25, effect: { status: "burn", chance: 0.1 } },
  flamewheel:{ id: "flamewheel", name: "Flame Wheel", type: "fire", power: 60, accuracy: 100, pp: 15, effect: { status: "burn", chance: 0.1 } },
  vinewhip: { id: "vinewhip", name: "Vine Whip", type: "grass",  power: 45, accuracy: 100, pp: 25 },
  spore:    { id: "spore",    name: "Spore",     type: "grass",  power: 0,  accuracy: 100, pp: 15, effect: { status: "sleep" } },
  stunspore:{ id: "stunspore",name: "Stun Spore",type: "grass",  power: 0,  accuracy: 90,  pp: 30, effect: { status: "paralysis" } },
  watergun: { id: "watergun", name: "Water Gun", type: "water",  power: 40, accuracy: 100, pp: 25 },
  bubble:   { id: "bubble",   name: "Bubble",    type: "water",  power: 40, accuracy: 100, pp: 30, effect: { stat: "speed", stages: -1, target: "foe", chance: 0.1 } },
  // Fallback move used when a creature has no usable PP left.
  struggle: { id: "struggle", name: "Struggle",  type: "normal", power: 50, accuracy: 100, pp: 1 },
};
