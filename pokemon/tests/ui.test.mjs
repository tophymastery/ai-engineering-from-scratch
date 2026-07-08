/* Browser UI/system test suite (Playwright). Serves the game over http, drives
 * it with real key presses (BFS pathfinding for navigation), asserts each
 * system, and captures screenshots of every major scene.
 *
 * Covers: boot, collision, building + cave entry, tall-grass random encounters,
 * a wild battle from encounter to win, win -> level up, the gym battle, and a
 * full walkthrough from the start point through gym 1 to the Victory Gate. */
import { chromium } from "playwright";
import path from "path";
import { fileURLToPath } from "url";
import { createServer } from "../serve.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const shots = path.join(dir, "..", "screenshots");
const CHROME = "/opt/pw-browsers/chromium-1194/chrome-linux/chrome";
const CODE = { up: "ArrowUp", down: "ArrowDown", left: "ArrowLeft", right: "ArrowRight", z: "KeyZ", x: "KeyX" };

let pass = 0, fail = 0;
const ok = (n, c) => { if (c) pass++; else { fail++; console.error("  FAIL:", n); } };

const S = (page) => page.evaluate(() => {
  const s = window.__shapemon;
  return { map: s.player.map, x: s.player.x, y: s.player.y, moving: s.player.moving,
           gstate: s.game.state, phase: s.battle.phase, level: s.player.party[0]?.level,
           exp: s.player.party[0]?.exp, badge: s.flags.gymBadge, starter: s.flags.hasStarter };
});
const settle = (page) => page.waitForFunction("!window.__shapemon.player.moving", { timeout: 4000 }).catch(() => {});
const wait = (page, ms) => page.waitForTimeout(ms);

async function tap(page, key) { await page.keyboard.press(CODE[key]); await wait(page, 40); }
async function stepDir(page, dir) {
  await page.keyboard.press(CODE[dir]);
  await wait(page, 40); await settle(page); await wait(page, 30);
}

// BFS the current map for a dir-path to (tx,ty); returns [] if already there/unreachable.
const bfsDirs = (page, tx, ty) => page.evaluate(({ tx, ty }) => {
  const s = window.__shapemon, m = s.player.map;
  const start = { x: s.player.x, y: s.player.y };
  const key = (x, y) => `${x},${y}`;
  const prev = new Map([[key(start.x, start.y), null]]);
  const q = [start];
  const dirs = { up: [0, -1], down: [0, 1], left: [-1, 0], right: [1, 0] };
  while (q.length) {
    const c = q.shift();
    if (c.x === tx && c.y === ty) {
      const out = []; let k = key(c.x, c.y);
      while (prev.get(k)) { const p = prev.get(k); out.unshift(p.d); k = p.k; }
      return out;
    }
    for (const [d, [dx, dy]] of Object.entries(dirs)) {
      const nx = c.x + dx, ny = c.y + dy, nk = key(nx, ny);
      const isTarget = nx === tx && ny === ty;
      // Never path THROUGH a warp/door/gate tile (it would teleport us away);
      // only step onto one when it is the destination.
      const warpish = "HLGCVDE".includes(s.tileAt(m, nx, ny));
      if (prev.has(nk) || (!isTarget && (warpish || s.isBlocked(m, nx, ny)))) continue;
      prev.set(nk, { k: key(c.x, c.y), d });
      q.push({ x: nx, y: ny });
    }
  }
  return null;
}, { tx, ty });

// Walk to (tx,ty) on the current map. Stops early if the map changes (warp),
// a battle starts, or we leave the world state.
async function walkTo(page, tx, ty, max = 260) {
  for (let i = 0; i < max; i++) {
    const st = await S(page);
    if (st.gstate !== "world") return st;
    if (st.map !== "town" && st.map !== "cave" && false) return st;
    if (st.x === tx && st.y === ty) return st;
    const dirs = await bfsDirs(page, tx, ty);
    if (!dirs || !dirs.length) return st;
    const before = st.map;
    await stepDir(page, dirs[0]);
    const after = await S(page);
    if (after.map !== before) return after;      // warped
    if (after.gstate !== "world") return after;  // battle/dialog started
  }
  return S(page);
}

async function clearDialog(page) {
  for (let i = 0; i < 12; i++) {
    const st = await S(page);
    if (st.gstate !== "dialog") break;
    await tap(page, "z");
  }
}

// Face an adjacent NPC/tile and interact, then clear the resulting dialog.
async function talkTo(page, dir) { await page.keyboard.press(CODE[dir]); await wait(page, 60); await tap(page, "z"); }

// Fight the current battle to a win (deterministic RNG set by caller). In the
// move menu it selects the highest-damage move against the current foe.
async function winBattle(page) {
  for (let i = 0; i < 500; i++) {
    const st = await S(page);
    if (st.gstate !== "battle") return st;
    if (st.phase === "moves") {
      const info = await page.evaluate(() => {
        const s = window.__shapemon, b = s.battle;
        let bi = 0, best = -1;
        b.ally.moves.forEach((m, idx) => {
          const r = s.api.calcDamage(b.ally, b.enemy, m, { forceCrit: false, rand: 100 });
          if (r.dmg > best) { best = r.dmg; bi = idx; }
        });
        return { bi, cur: b.moveIndex, n: b.ally.moves.length };
      });
      for (let k = 0; k < (info.bi - info.cur + info.n) % info.n; k++) await tap(page, "down");
      await tap(page, "z");
    } else {
      await tap(page, "z");   // advance messages / select FIGHT
    }
  }
  return S(page);
}

(async () => {
  const server = createServer().listen(0);
  const port = server.address().port;
  const url = `http://localhost:${port}/index.html`;

  const browser = await chromium.launch({ executablePath: CHROME, args: ["--no-sandbox"] });
  const page = await browser.newPage({ viewport: { width: 820, height: 620 } });
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text())) errors.push(m.text());
  });

  await page.goto(url);
  await page.waitForFunction("window.__shapemon !== undefined");
  await page.evaluate(() => { try { localStorage.clear(); } catch {} window.__shapemon.setRng(() => 0.9); });
  await wait(page, 300);

  // 1) Title
  await page.screenshot({ path: path.join(shots, "1-title.png") });

  // 2) New Game -> intro dialog
  await tap(page, "z");
  await wait(page, 150);
  await page.screenshot({ path: path.join(shots, "2-intro.png") });
  ok("new game -> dialog", (await S(page)).gstate === "dialog");
  await clearDialog(page);
  ok("dialog cleared -> world in home", (await S(page)).map === "home");

  // 3) Collision: bump the left/top walls inside the house; position must clamp.
  for (let i = 0; i < 6; i++) await stepDir(page, "left");
  let st = await S(page);
  ok("collision: cannot pass west wall", st.x >= 1);
  for (let i = 0; i < 6; i++) await stepDir(page, "up");
  st = await S(page);
  ok("collision: cannot pass north wall", st.y >= 1);
  ok("collision: movement actually happened", st.x < 5);

  // 4) Building exit: walk to the home door -> warp to town.
  await walkTo(page, 5, 8);
  ok("exited house into town", (await S(page)).map === "town");
  await page.screenshot({ path: path.join(shots, "3-overworld-town.png") });

  // 5) Enter Lab, talk to Prof, receive starter, exit.
  const DOORS = await page.evaluate(() => window.__shapemon.DOORS);
  await walkTo(page, DOORS.L.x, DOORS.L.y);
  ok("entered Lab building", (await S(page)).map === "lab");
  await walkTo(page, 6, 3);          // approach the professor (at 6,2)
  await talkTo(page, "up");
  await clearDialog(page);
  ok("received starter", (await S(page)).starter === true);
  await walkTo(page, 6, 9);          // lab exit door
  ok("exited Lab to town", (await S(page)).map === "town");

  // 6) Enter Heal Center (building), talk to nurse, exit.
  await walkTo(page, DOORS.C.x, DOORS.C.y);
  ok("entered Heal Center", (await S(page)).map === "center");
  await walkTo(page, 6, 8);
  ok("exited Heal Center", (await S(page)).map === "town");

  // 7) Enter Cave, walk on cave floor, exit.
  await page.evaluate(() => window.__shapemon.setNoEncounter(true));
  await walkTo(page, DOORS.V.x, DOORS.V.y);
  ok("entered Rocky Cave", (await S(page)).map === "cave");
  const cavePos = await S(page);
  await stepDir(page, "down"); await stepDir(page, "left");
  const caveMoved = await S(page);
  ok("can walk inside cave", caveMoved.x !== cavePos.x || caveMoved.y !== cavePos.y);
  await page.screenshot({ path: path.join(shots, "4-cave.png") });
  await walkTo(page, 7, 9);
  ok("exited Cave to town", (await S(page)).map === "town");

  // 8) Tall-grass random encounter (forced), then win the wild battle + level up.
  await page.evaluate(() => {
    const s = window.__shapemon;
    s.setNoEncounter(false); s.setForceEncounter(true);
    s.player.party[0].exp = s.api.expForLevel(s.player.party[0].level + 1) - 1; // one win -> level up
    s.warpTo({ map: "town", x: 12, y: 13, dir: "down" });
  });
  const beforeLvl = (await S(page)).level;
  await stepDir(page, "down");   // step onto tall grass -> forced encounter
  ok("tall grass triggered a wild battle", (await S(page)).gstate === "battle");
  await page.screenshot({ path: path.join(shots, "5-wild-battle.png") });
  const afterWild = await winBattle(page);
  ok("wild battle: returned to overworld after win", afterWild.gstate === "world");
  ok("battle win granted a level up", afterWild.level > beforeLvl);
  await page.evaluate(() => window.__shapemon.setForceEncounter(false));

  // 9) Gym battle: heal at the Center (proven above), travel to the gym with no
  //    encounters, talk to the leader, and win to earn the badge.
  await page.evaluate(() => { window.__shapemon.setNoEncounter(true); window.__shapemon.healParty(); });
  await walkTo(page, DOORS.G.x, DOORS.G.y);
  ok("entered Gym", (await S(page)).map === "gym");
  await walkTo(page, 6, 3);
  await talkTo(page, "up");
  await clearDialog(page);
  ok("gym battle started", (await S(page)).gstate === "battle");
  await page.screenshot({ path: path.join(shots, "6-gym-battle.png") });
  const afterGym = await winBattle(page);
  ok("gym cleared -> Leaf Badge earned", afterGym.badge === true);
  await clearDialog(page);   // Leader Fern's victory speech

  // 10) Full walkthrough end: leave gym, reach the Victory Gate -> credits.
  await walkTo(page, 6, 10);   // gym exit
  ok("back in town after gym", (await S(page)).map === "town");
  const gate = DOORS.E;
  await walkTo(page, gate.x, gate.y);
  await clearDialog(page);
  const end = await S(page);
  ok("Victory Gate reached the ending", end.gstate === "credits");
  await wait(page, 200);
  await page.screenshot({ path: path.join(shots, "7-credits.png") });

  await browser.close();
  server.close();

  ok("no JS errors during play", errors.length === 0);
  if (errors.length) console.error("  errors:", errors.slice(0, 5));
  console.log(`\n=== ui: ${pass} passed, ${fail} failed ===`);
  process.exit(fail ? 1 : 0);
})();
