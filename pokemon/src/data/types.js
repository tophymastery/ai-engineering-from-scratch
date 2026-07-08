/* Elemental types: colors, the physical/special split, and the type chart.
 * Add a type by adding a color, listing it in PHYSICAL_TYPES if it's physical,
 * and adding its row + a column in every row of TYPE_CHART. */

export const TYPE_COLORS = {
  fire: "#f0862c",
  water: "#3f9fff",
  grass: "#5bd06a",
  normal: "#c9c9c9",
};

// In Gen-3, a move's damage category is decided by its TYPE.
// Of the four types used here, only "normal" is physical.
export const PHYSICAL_TYPES = ["normal"];

// TYPE_CHART[attacking][defending] = damage multiplier (Gen-1/3 values).
export const TYPE_CHART = {
  fire:   { fire: 0.5, water: 0.5, grass: 2.0, normal: 1.0 },
  water:  { fire: 2.0, water: 0.5, grass: 0.5, normal: 1.0 },
  grass:  { fire: 0.5, water: 2.0, grass: 0.5, normal: 1.0 },
  normal: { fire: 1.0, water: 1.0, grass: 1.0, normal: 1.0 },
};

export const typeEffectiveness = (atk, def) =>
  (TYPE_CHART[atk] && TYPE_CHART[atk][def] != null) ? TYPE_CHART[atk][def] : 1.0;

export const isSpecial = (type) => !PHYSICAL_TYPES.includes(type);
