/* Playwright harness: verifies the game runs and captures screenshots of each
 * major scene (title, overworld, battle, victory/credits). Also runs a small
 * logic check that a full battle drives the win -> credits transition. */
import { chromium } from "playwright";
import { fileURLToPath } from "url";
import path from "path";

const dir = path.dirname(fileURLToPath(import.meta.url));
const url = "file://" + path.join(dir, "index.html");
const shots = path.join(dir, "screenshots");

const key = async (page, code) => {
  await page.keyboard.press(code);
  await page.waitForTimeout(120);
};
const wait = (page, ms) => page.waitForTimeout(ms);

(async () => {
  const browser = await chromium.launch({
    executablePath: "/opt/pw-browsers/chromium-1194/chrome-linux/chrome",
    args: ["--no-sandbox"],
  });
  const page = await browser.newPage({ viewport: { width: 640, height: 640 } });

  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });

  await page.goto(url);
  await page.waitForFunction("window.__shapemon !== undefined");
  await wait(page, 400);

  // 1) Title
  await page.screenshot({ path: path.join(shots, "1-title.png") });
  console.log("captured title");

  // 2) Start -> intro dialog
  await key(page, "Enter");
  await wait(page, 200);
  await page.screenshot({ path: path.join(shots, "2-intro-dialog.png") });
  console.log("captured intro dialog");

  // clear the intro dialog
  for (let i = 0; i < 6; i++) await key(page, "KeyZ");

  // Prove movement + collision: try to walk into the top wall of the room,
  // then walk down toward the exit door.
  const startPos = await page.evaluate(() => ({ ...window.__shapemon.player, party: null }));
  for (let i = 0; i < 4; i++) await key(page, "ArrowUp"); // into wall — should be blocked
  const afterUp = await page.evaluate(() => window.__shapemon.player.y);
  console.log(`collision check: startY=${startPos.y} afterWallPushY=${afterUp} (expect unchanged/clamped)`);

  // 3) Overworld: put the player in town with a starter for a representative shot
  await page.evaluate(() => {
    window.__shapemon.giveStarter();
    window.__shapemon.warpTo({ map: "town", x: 7, y: 8, dir: "up" });
    window.__shapemon.game.state = window.__shapemon.STATE.WORLD;
  });
  await wait(page, 200);
  await page.screenshot({ path: path.join(shots, "3-overworld.png") });
  console.log("captured overworld");

  // 4) Battle scene (gym battle)
  await page.evaluate(() => window.__shapemon.startGymBattle());
  await wait(page, 150);
  await key(page, "KeyZ"); // advance "sent out" lines
  await key(page, "KeyZ");
  await wait(page, 150);
  await page.screenshot({ path: path.join(shots, "4-battle.png") });
  console.log("captured battle menu");

  // show move list
  await key(page, "KeyZ"); // FIGHT
  await wait(page, 150);
  await page.screenshot({ path: path.join(shots, "5-battle-moves.png") });
  console.log("captured battle moves");

  // 5) Logic: fight to a win and confirm we reach CREDITS.
  let guard = 0;
  while (guard++ < 60) {
    const st = await page.evaluate(() => {
      const s = window.__shapemon;
      return { state: s.game.state, phase: s.battle.phase, enemyHp: s.battle.enemy && s.battle.enemy.hp };
    });
    if (st.state === "credits") break;
    if (st.state !== "battle") break;
    if (st.phase === "menu") { await key(page, "KeyZ"); }       // FIGHT
    else if (st.phase === "moves") { await key(page, "KeyZ"); } // pick Scratch
    else if (st.phase === "anim") { await key(page, "KeyZ"); }  // advance messages
    else await key(page, "KeyZ");
  }

  const finalState = await page.evaluate(() => window.__shapemon.game.state);
  console.log(`final state after battle loop: ${finalState}`);
  await wait(page, 300);
  await page.screenshot({ path: path.join(shots, "6-credits.png") });
  console.log("captured credits");

  await browser.close();

  console.log("\n=== RESULTS ===");
  console.log("JS errors:", errors.length ? errors : "none");
  const ok = finalState === "credits" && errors.length === 0;
  console.log("Win -> credits reached:", finalState === "credits");
  console.log(ok ? "PASS" : "FAIL");
  process.exit(ok ? 0 : 1);
})();
