/* The current story objective, derived from progress flags. Gives the game a
 * clear, guided arc from start to finish (shown in the overworld sidebar). */
import { flags, player } from "../state.js";
import { GYM_BADGES } from "../data/maps.js";

export function objective() {
  if (!flags.hasStarter) return "Visit Prof. Cedar's Lab for your starter";
  const g = GYM_BADGES.find((gm) => !flags.badges[gm.badge]);
  if (!g) return "Enter the Victory Gate to finish your journey!";
  return player.map === g.region
    ? `Beat ${g.leader} at the ${g.town} Gym`
    : `Travel to ${g.town} and challenge its Gym`;
}
