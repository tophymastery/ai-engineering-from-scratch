/* Wild encounter tables, keyed by map name. Edit freely.
 *   rate  : chance per step on an encounter tile (tall grass / cave floor)
 *   table : weighted list of { species, min, max } level ranges */
export const ENCOUNTERS = {
  town: {
    rate: 0.16,
    table: [
      { species: "wormling", min: 3, max: 4, weight: 5 },
      { species: "nibbit",   min: 3, max: 4, weight: 5 },
    ],
  },
  cave: {
    rate: 0.20,
    table: [
      { species: "cavvit", min: 5, max: 7, weight: 6 },
      { species: "nibbit", min: 4, max: 6, weight: 4 },
    ],
  },
  north: {
    rate: 0.16,
    table: [
      { species: "dribblet", min: 8, max: 11, weight: 6 },
      { species: "wormling", min: 8, max: 10, weight: 4 },
    ],
  },
  east: {
    rate: 0.16,
    table: [
      { species: "cavvit", min: 13, max: 16, weight: 5 },
      { species: "nibbit", min: 12, max: 15, weight: 5 },
    ],
  },
};
