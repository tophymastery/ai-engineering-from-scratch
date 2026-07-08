/* Title screen (with New Game / Continue menu) and end credits. */
import { ctx, W, H } from "../core/screen.js";
import { game } from "../state.js";
import { makeCreature } from "../engine/creature.js";
import { hasSave } from "../engine/save.js";
import { CREDITS } from "../data/story.js";

export const titleOptions = () => (hasSave() ? ["Continue", "New Game"] : ["New Game"]);

export function renderTitle() {
  const g = ctx.createLinearGradient(0, 0, 0, H);
  g.addColorStop(0, "#1a1c3a"); g.addColorStop(1, "#3a1c2a");
  ctx.fillStyle = g; ctx.fillRect(0, 0, W, H);
  drawCreature("emberling");
  ctx.fillStyle = "#f0862c"; ctx.font = "bold 44px 'Courier New', monospace";
  ctx.textAlign = "center"; ctx.textBaseline = "middle";
  ctx.fillText("SHAPEMON", W / 2, 150);
  ctx.fillStyle = "#f4f4ec"; ctx.font = "22px 'Courier New', monospace";
  ctx.fillText("Ember Quest", W / 2, 192);

  const opts = titleOptions();
  opts.forEach((o, i) => {
    ctx.fillStyle = i === game.titleIndex ? "#ffd23f" : "#c9c9e0";
    ctx.font = "22px 'Courier New', monospace";
    ctx.fillText((i === game.titleIndex ? "> " : "  ") + o, W / 2, 290 + i * 40);
  });
  ctx.fillStyle = "#8fb6ff"; ctx.font = "14px 'Courier New', monospace";
  ctx.fillText("Up/Down to choose  ·  Z/Enter to select", W / 2, 430);
  ctx.textAlign = "left";

  function drawCreature() {
    const cr = makeCreature("emberling", 5);
    ctx.fillStyle = cr.sprite.color;
    ctx.beginPath(); ctx.moveTo(W / 2, 40);
    ctx.quadraticCurveTo(W / 2 + 44, 90, W / 2, 110);
    ctx.quadraticCurveTo(W / 2 - 44, 90, W / 2, 40); ctx.fill();
    ctx.fillStyle = "#ffd23f"; ctx.beginPath(); ctx.arc(W / 2, 92, 16, 0, 7); ctx.fill();
  }
}

export function renderCredits() {
  const g = ctx.createLinearGradient(0, 0, 0, H);
  g.addColorStop(0, "#101427"); g.addColorStop(1, "#2a1030");
  ctx.fillStyle = g; ctx.fillRect(0, 0, W, H);
  ctx.textAlign = "center"; ctx.textBaseline = "top";
  CREDITS.forEach((line, i) => {
    ctx.fillStyle = i === 0 ? "#ffd23f" : "#f4f4ec";
    ctx.font = (i === 0 ? "bold 22px" : "16px") + " 'Courier New', monospace";
    ctx.fillText(line, W / 2, 70 + i * 24);
  });
  ctx.textAlign = "left";
}
