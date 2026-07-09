/* The current story objective, derived from progress flags. Gives the game a
 * clear, guided arc from start to finish (shown in the overworld sidebar). */
import { flags, player } from "../state.js";

export function objective() {
  if (!flags.hasStarter) return "Visit Prof. Cedar's Lab for your starter";
  if (!flags.badges[0]) return "Earn the Leaf Badge at Fernwood Gym";
  if (!flags.badges[1]) {
    return player.map === "north"
      ? "Beat Leader Marina at the Tidewater Gym"
      : "Head north through the gate to Tidewater Town";
  }
  return "Enter the Victory Gate to finish your journey!";
}
