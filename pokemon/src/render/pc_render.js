/* Storage PC screen: PARTY panel (left) and BOX panel (right); move creatures
 * between them. */
import { ctx, W, H } from "../core/screen.js";
import { player, pc } from "../state.js";
import { drawBox } from "./hud.js";

function panel(title, list, x, y, w, h, active, index) {
  drawBox(x, y, w, h, 8);
  ctx.fillStyle = "#20222f"; ctx.textBaseline = "top"; ctx.textAlign = "left";
  ctx.font = "bold 16px 'Courier New', monospace"; ctx.fillText(title, x + 16, y + 12);
  ctx.font = "15px 'Courier New', monospace";
  if (!list.length) { ctx.fillStyle = "#9a9aa6"; ctx.fillText("(empty)", x + 20, y + 44); }
  list.slice(0, 8).forEach((c, i) => {
    const sel = active && i === index;
    ctx.fillStyle = sel ? "#d63c3c" : (c.hp > 0 ? "#20222f" : "#a0a0a8");
    ctx.fillText((sel ? "▶ " : "  ") + `${c.name} Lv${c.level}`, x + 16, y + 42 + i * 22);
  });
}

export function renderPc() {
  ctx.fillStyle = "#1a2138"; ctx.fillRect(0, 0, W, H);
  ctx.fillStyle = "#e9e9f2"; ctx.textAlign = "center"; ctx.textBaseline = "top";
  ctx.font = "bold 20px 'Courier New', monospace"; ctx.fillText("STORAGE PC", W / 2, 20);
  ctx.textAlign = "left";

  const pw = (W - 60) / 2, ph = H - 130;
  panel(`PARTY (${player.party.length}/6)`, player.party, 20, 60, pw, ph, pc.panel === 0, pc.index);
  panel(`BOX (${player.box.length})`, player.box, 40 + pw, 60, pw, ph, pc.panel === 1, pc.index);

  drawBox(20, H - 60, W - 40, 46, 8);
  ctx.fillStyle = "#20222f"; ctx.font = "14px 'Courier New', monospace"; ctx.textBaseline = "top";
  ctx.fillText(pc.panel === 0 ? "Z = deposit to box" : "Z = withdraw to party", 34, H - 46);
  ctx.fillStyle = "#3553ff"; ctx.textAlign = "right"; ctx.fillText("←/→ switch  ·  X = exit", W - 34, H - 46); ctx.textAlign = "left";
}
