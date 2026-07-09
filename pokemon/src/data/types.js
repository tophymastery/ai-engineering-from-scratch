/* Elemental types: colors, the physical/special split, and the type chart.
 * Nine types back the eight gyms. Rows are sparse — any pairing not listed is
 * neutral (x1). Add a type by adding a color, listing it in PHYSICAL_TYPES if
 * physical, and adding its (sparse) row. */

export const TYPE_COLORS = {
  normal: "#c9c9c9", fire: "#f0862c", water: "#3f9fff", grass: "#5bd06a",
  electric: "#f7d02c", rock: "#b8a038", ice: "#8fd6e0", psychic: "#f85888", poison: "#a86fd0",
};

// Gen-3 damage category is by TYPE. Physical types among ours:
export const PHYSICAL_TYPES = ["normal", "rock", "poison"];

// TYPE_CHART[attacking][defending] = multiplier (only non-1x entries listed).
export const TYPE_CHART = {
  normal:   { rock: 0.5 },
  fire:     { fire: 0.5, water: 0.5, grass: 2, rock: 0.5, ice: 2 },
  water:    { fire: 2, water: 0.5, grass: 0.5, rock: 2 },
  grass:    { fire: 0.5, water: 2, grass: 0.5, rock: 2, poison: 0.5 },
  electric: { water: 2, grass: 0.5, electric: 0.5 },
  rock:     { fire: 2, ice: 2 },
  ice:      { fire: 0.5, water: 0.5, grass: 2, ice: 0.5 },
  psychic:  { psychic: 0.5, poison: 2 },
  poison:   { grass: 2, rock: 0.5, poison: 0.5 },
};

export const typeEffectiveness = (atk, def) =>
  (TYPE_CHART[atk] && TYPE_CHART[atk][def] != null) ? TYPE_CHART[atk][def] : 1.0;

export const isSpecial = (type) => !PHYSICAL_TYPES.includes(type);
