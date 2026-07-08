/* Dialog queue. Any module can push lines; advancing past the last line runs an
 * optional callback (used to grant the starter, start a battle, heal, etc.). */
import { game, dialog, STATE } from "../state.js";

export function say(lines, after, speaker) {
  dialog.lines = Array.isArray(lines) ? lines : [lines];
  dialog.index = 0;
  dialog.active = true;
  dialog.speaker = speaker || null;
  game.afterDialog = after || null;
  game.state = STATE.DIALOG;
}

export function advanceDialog() {
  dialog.index++;
  if (dialog.index >= dialog.lines.length) {
    dialog.active = false;
    const cb = game.afterDialog;
    game.afterDialog = null;
    if (game.state === STATE.DIALOG) game.state = STATE.WORLD;
    if (cb) cb();
  }
}
