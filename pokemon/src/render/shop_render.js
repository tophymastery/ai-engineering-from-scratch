/* Mart / shop screen: scrollable item list with prices and your wallet. */
import { ctx, W, H } from "../core/screen.js";
import { player, shop } from "../state.js";
import { ITEMS } from "../data/items.js";
import { drawBox, wrap } from "./hud.js";

export function renderShop() {
  ctx.fillStyle = "#20263a"; ctx.fillRect(0, 0, W, H);

  drawBox(12, 12, W - 24, 48);
  ctx.fillStyle = "#20222f"; ctx.textBaseline = "top"; ctx.textAlign = "left";
  ctx.font = "bold 20px 'Courier New', monospace"; ctx.fillText("MART", 30, 24);
  ctx.textAlign = "right"; ctx.font = "18px 'Courier New', monospace";
  ctx.fillText(`$ ${player.money}`, W - 30, 26); ctx.textAlign = "left";

  drawBox(12, 72, W - 24, H - 150);
  ctx.font = "18px 'Courier New', monospace";
  shop.stock.forEach((id, i) => {
    const it = ITEMS[id], sel = i === shop.index, afford = player.money >= it.price;
    ctx.fillStyle = sel ? "#d63c3c" : (afford ? "#20222f" : "#9a9aa6");
    ctx.fillText((sel ? "> " : "  ") + it.name, 36, 88 + i * 30);
    ctx.textAlign = "right"; ctx.fillText(`$${it.price}`, W - 40, 88 + i * 30); ctx.textAlign = "left";
  });

  drawBox(12, H - 72, W - 24, 60);
  ctx.fillStyle = "#20222f"; ctx.font = "15px 'Courier New', monospace";
  const desc = ITEMS[shop.stock[shop.index]].desc;
  wrap(desc, W - 60).slice(0, 1).forEach((ln) => ctx.fillText(ln, 30, H - 62));
  ctx.fillStyle = "#3553ff"; ctx.font = "14px 'Courier New', monospace";
  ctx.fillText("Z = buy   ·   X = leave", 30, H - 38);
}
