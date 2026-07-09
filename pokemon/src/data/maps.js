/* Maps: two scrolling overworld towns + interiors. Grid stays tile-based.
 * Tile legend:
 *   '.' grass  '_' path  ':' tall grass  'T' tree(blocked)  '#' roof  'b' blue roof
 *   '=' interior wall  'R' rock(blocked)  '~' water(blocked)  'F' floor  'D' interior exit
 *   'H' home  'L' lab  'G' gym  'C' heal center  'V' cave  'M' mart  'E' gate
 *
 * Overworlds are open grass fields with obstacles you walk AROUND, so buildings
 * stay reachable. A reachability test guards this in the suite. */

const inBounds = (g, x, y) => y >= 0 && y < g.length && x >= 0 && x < g[0].length;
const rect = (g, x0, y0, x1, y1, ch, skip) => {
  for (let y = y0; y <= y1; y++) for (let x = x0; x <= x1; x++)
    if (inBounds(g, x, y) && (!skip || skip(g[y][x]))) g[y][x] = ch;
};
const blank = (w, h) => Array.from({ length: h }, () => Array.from({ length: w }, () => "."));
const border = (g) => {
  const w = g[0].length, h = g.length;
  rect(g, 0, 0, w - 1, 1, "T"); rect(g, 0, h - 2, w - 1, h - 1, "T");
  rect(g, 0, 0, 1, h - 1, "T"); rect(g, w - 2, 0, w - 1, h - 1, "T");
};
const onlyGrass = (c) => c === ".";

// ---- Willow Town (start) ---------------------------------------------------
function buildTown() {
  const w = 32, h = 44, g = blank(w, h), doors = {};
  border(g);
  rect(g, 15, 4, 16, h - 4, "_");
  rect(g, 7, 7, 16, 8, "_");
  rect(g, 7, 39, 23, 40, "_");
  rect(g, 16, 11, 25, 12, "_");
  const building = (x, ry, ch, blue) => { rect(g, x, ry, x + 2, ry, blue ? "b" : "#"); g[ry + 1][x + 1] = ch; doors[ch] = { x: x + 1, y: ry + 1 }; };
  building(15, 3, "G");        // Gym (Fern)
  building(6, 6, "C", true);   // Heal Center
  building(6, 38, "H");        // Home
  building(21, 38, "L");       // Lab
  building(3, 10, "M");        // Mart (shop)
  building(10, 2, "E");        // Victory Gate -> Tidewater Town
  rect(g, 23, 9, 25, 9, "R"); g[10][23] = "R"; g[10][25] = "R"; g[10][24] = "V"; doors["V"] = { x: 24, y: 10 };
  rect(g, 3, 20, 9, 27, "~");
  rect(g, 10, 14, 14, 18, ":"); rect(g, 19, 21, 24, 26, ":"); rect(g, 11, 30, 16, 34, ":"); rect(g, 22, 15, 26, 18, ":");
  const clusters = [[10, 4, 13, 6], [26, 20, 28, 24], [3, 32, 6, 36], [26, 30, 29, 35], [2, 14, 4, 17], [27, 4, 29, 8], [9, 24, 12, 27], [17, 33, 20, 36], [24, 27, 27, 29]];
  for (const [x0, y0, x1, y1] of clusters) rect(g, x0, y0, x1, y1, "T", onlyGrass);
  return { grid: g, doors };
}

// ---- Post-gym regions (Tidewater, Cinder) share this proven layout ---------
function buildRegion() {
  const w = 24, h = 30, g = blank(w, h), doors = {};
  border(g);
  rect(g, 10, 3, 11, h - 4, "_");
  rect(g, 4, 7, 11, 8, "_");
  const building = (x, ry, ch, blue) => { rect(g, x, ry, x + 2, ry, blue ? "b" : "#"); g[ry + 1][x + 1] = ch; doors[ch] = { x: x + 1, y: ry + 1 }; };
  building(9, 3, "G");         // Gym 2 (Marina)
  building(3, 6, "C", true);   // Heal Center
  building(14, 2, "E");        // Final Victory Gate -> credits
  rect(g, 3, 17, 9, 25, "~");  // big tidewater pond
  rect(g, 13, 10, 17, 14, ":"); rect(g, 14, 20, 19, 24, ":");
  const clusters = [[3, 3, 5, 5], [18, 6, 20, 9], [18, 18, 20, 22], [5, 12, 7, 14]];
  for (const [x0, y0, x1, y1] of clusters) rect(g, x0, y0, x1, y1, "T", onlyGrass);
  return { grid: g, doors };
}

const INTERIOR9 = (door = 6) => ["=============", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "======D======"];
const INTERIOR11 = ["=============", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "=FFFFFFFFFFF=", "======D======"];

const RAW_INTERIOR = {
  home: ["===========", "=FFFFFFFFF=", "=FFFFFFFFF=", "=FFFFFFFFF=", "=FFFFFFFFF=", "=FFFFFFFFF=", "=FFFFFFFFF=", "=FFFFFFFFF=", "=====D====="],
  lab: INTERIOR9(),
  center: INTERIOR9(),
  center2: INTERIOR9(),
  center3: INTERIOR9(),
  mart: INTERIOR9(),
  gym: INTERIOR11,
  gym2: INTERIOR11,
  gym3: INTERIOR11,
  cave: ["===============", "=FFFFFFFFFFFFF=", "=FF=FFFFFFF=FF=", "=FFFFFFFFFFFFF=", "=FF=FFFFFFF=FF=", "=FFFFFFFFFFFFF=", "=FF=FFFFFFF=FF=", "=FFFFFFFFFFFFF=", "=FFFFFFFFFFFFF=", "=======D=======", "==============="],
};

export const MAPS = {};
function registerMap(name, grid, theme) {
  const w = grid[0].length;
  for (const row of grid) if (row.length !== w) console.error(`Map ${name} width mismatch: ${row.length} != ${w}`);
  MAPS[name] = { name, grid, w, h: grid.length, theme };
}

const townB = buildTown(), northB = buildRegion(), eastB = buildRegion();
registerMap("town", townB.grid, "town");
registerMap("north", northB.grid, "town");
registerMap("east", eastB.grid, "town");
for (const name in RAW_INTERIOR)
  registerMap(name, RAW_INTERIOR[name].map((r) => r.split("")), name === "cave" ? "cave" : "interior");

const D = townB.doors, ND = northB.doors, ED = eastB.doors;
// Interior exit-door coordinates.
const EXIT = { home: [5, 8], lab: [6, 8], center: [6, 8], center2: [6, 8], center3: [6, 8], mart: [6, 8], gym: [6, 10], gym2: [6, 10], gym3: [6, 10], cave: [7, 9] };

export const WARPS = {
  [`town:${D.H.x},${D.H.y}`]: { map: "home", x: 5, y: 7, dir: "up" },
  [`town:${D.L.x},${D.L.y}`]: { map: "lab", x: 6, y: 7, dir: "up" },
  [`town:${D.C.x},${D.C.y}`]: { map: "center", x: 6, y: 7, dir: "up" },
  [`town:${D.G.x},${D.G.y}`]: { map: "gym", x: 6, y: 9, dir: "up" },
  [`town:${D.M.x},${D.M.y}`]: { map: "mart", x: 6, y: 7, dir: "up" },
  [`town:${D.V.x},${D.V.y}`]: { map: "cave", x: 7, y: 8, dir: "up" },
  [`north:${ND.G.x},${ND.G.y}`]: { map: "gym2", x: 6, y: 9, dir: "up" },
  [`north:${ND.C.x},${ND.C.y}`]: { map: "center2", x: 6, y: 7, dir: "up" },
  [`east:${ED.G.x},${ED.G.y}`]: { map: "gym3", x: 6, y: 9, dir: "up" },
  [`east:${ED.C.x},${ED.C.y}`]: { map: "center3", x: 6, y: 7, dir: "up" },
  [`home:${EXIT.home.join(",")}`]: { map: "town", x: D.H.x, y: D.H.y + 1, dir: "down" },
  [`lab:${EXIT.lab.join(",")}`]: { map: "town", x: D.L.x, y: D.L.y + 1, dir: "down" },
  [`center:${EXIT.center.join(",")}`]: { map: "town", x: D.C.x, y: D.C.y + 1, dir: "down" },
  [`gym:${EXIT.gym.join(",")}`]: { map: "town", x: D.G.x, y: D.G.y + 1, dir: "down" },
  [`mart:${EXIT.mart.join(",")}`]: { map: "town", x: D.M.x, y: D.M.y + 1, dir: "down" },
  [`cave:${EXIT.cave.join(",")}`]: { map: "town", x: D.V.x, y: D.V.y + 1, dir: "down" },
  [`gym2:${EXIT.gym2.join(",")}`]: { map: "north", x: ND.G.x, y: ND.G.y + 1, dir: "down" },
  [`center2:${EXIT.center2.join(",")}`]: { map: "north", x: ND.C.x, y: ND.C.y + 1, dir: "down" },
  [`gym3:${EXIT.gym3.join(",")}`]: { map: "east", x: ED.G.x, y: ED.G.y + 1, dir: "down" },
  [`center3:${EXIT.center3.join(",")}`]: { map: "east", x: ED.C.x, y: ED.C.y + 1, dir: "down" },
};

// Badge-gated gates. On stepping onto an 'E' tile the engine looks up the gate
// for the current map: warp onward if the required badge is earned, else lock.
export const GATES = {
  town: { need: 0, warp: { map: "north", x: 11, y: 27, dir: "up" } },
  north: { need: 1, warp: { map: "east", x: 11, y: 27, dir: "up" } },
  east: { need: 2, credits: true },
};

export const NPCS = {
  lab: [{ x: 6, y: 2, color: "#e6e6e6", role: "prof", name: "Prof. Cedar" }],
  center: [{ x: 6, y: 2, color: "#ff8fb0", role: "nurse", name: "Nurse" }, { x: 3, y: 2, color: "#8fd0ff", role: "pc", name: "Storage PC" }],
  center2: [{ x: 6, y: 2, color: "#ff8fb0", role: "nurse", name: "Nurse" }, { x: 3, y: 2, color: "#8fd0ff", role: "pc", name: "Storage PC" }],
  center3: [{ x: 6, y: 2, color: "#ff8fb0", role: "nurse", name: "Nurse" }, { x: 3, y: 2, color: "#8fd0ff", role: "pc", name: "Storage PC" }],
  mart: [{ x: 6, y: 2, color: "#8fd0ff", role: "shop", name: "Clerk" }],
  gym: [{ x: 6, y: 2, color: "#2f9e57", role: "gym", name: "Leader Fern", badge: 0, intro: "gymIntro", done: "gymDone",
          party: [{ species: "thornbud", level: 6 }] }],
  gym2: [{ x: 6, y: 2, color: "#2f7fe0", role: "gym", name: "Leader Marina", badge: 1, intro: "gym2Intro", done: "gym2Done",
           party: [{ species: "dribblet", level: 11 }, { species: "torrentyl", level: 13 }] }],
  gym3: [{ x: 6, y: 2, color: "#c9a06a", role: "gym", name: "Leader Rocco", badge: 2, intro: "gym3Intro", done: "gym3Done",
           party: [{ species: "nibbit", level: 16 }, { species: "cavvit", level: 18 }] }],
  town: [
    { x: 10, y: 41, color: "#ffd166", role: "villager", dialog: "kid", name: "Kid" },
    { x: 20, y: 41, color: "#b0855b", role: "villager", dialog: "oldman", name: "Old Man" },
    { x: 20, y: 13, color: "#7a9cc6", role: "villager", dialog: "hiker", name: "Hiker" },
    { x: 17, y: 19, color: "#e07a3f", role: "trainer", dialog: "rick", name: "Camper Rick", party: [{ species: "nibbit", level: 4 }], defeated: false },
    { x: 13, y: 28, color: "#c85fb0", role: "trainer", dialog: "mia", name: "Scout Mia", party: [{ species: "wormling", level: 4 }, { species: "nibbit", level: 5 }], defeated: false },
  ],
  north: [
    { x: 13, y: 26, color: "#8fd0ff", role: "villager", dialog: "swimmer", name: "Swimmer" },
    { x: 16, y: 22, color: "#3f9fff", role: "trainer", dialog: "kai", name: "Sailor Kai", party: [{ species: "dribblet", level: 10 }], defeated: false },
  ],
  east: [
    { x: 13, y: 26, color: "#d0b070", role: "villager", dialog: "elder", name: "Elder" },
    { x: 16, y: 22, color: "#c98a3a", role: "trainer", dialog: "bruno", name: "Ranger Bruno", party: [{ species: "cavvit", level: 15 }, { species: "nibbit", level: 15 }], defeated: false },
  ],
};

export const DOORS = D;
export const NORTH_DOORS = ND;
export const EAST_DOORS = ED;

// ---- Gyms 4-8: five more regions, wired via the generic region builder -----
const LATE = [
  { region: "r4", idx: 4, town: "Voltage City", badge: 3, leader: "Leader Volt",  mon: "voltling", lv: [20, 22] },
  { region: "r5", idx: 5, town: "Stonehaven",   badge: 4, leader: "Leader Terra",  mon: "pebblo",   lv: [24, 26] },
  { region: "r6", idx: 6, town: "Glacia Town",  badge: 5, leader: "Leader Frost",  mon: "frostpup", lv: [28, 30] },
  { region: "r7", idx: 7, town: "Mindspire",    badge: 6, leader: "Leader Sage",   mon: "mindly",   lv: [32, 34] },
  { region: "r8", idx: 8, town: "Miasma Marsh", badge: 7, leader: "Leader Venia",  mon: "venuff",   lv: [36, 38] },
];
// The gate leading INTO the first late region sits on "east".
GATES.east = { need: 2, warp: { map: "r4", x: 11, y: 27, dir: "up" } };
LATE.forEach((g, i) => {
  const rb = buildRegion(), d = rb.doors;
  registerMap(g.region, rb.grid, "town");
  const gymMap = `gym${g.idx}`, centerMap = `center${g.idx}`;
  registerMap(gymMap, INTERIOR11.map((r) => r.split("")), "interior");
  registerMap(centerMap, INTERIOR9().map((r) => r.split("")), "interior");
  EXIT[gymMap] = [6, 10]; EXIT[centerMap] = [6, 8];
  WARPS[`${g.region}:${d.G.x},${d.G.y}`] = { map: gymMap, x: 6, y: 9, dir: "up" };
  WARPS[`${g.region}:${d.C.x},${d.C.y}`] = { map: centerMap, x: 6, y: 7, dir: "up" };
  WARPS[`${gymMap}:${EXIT[gymMap].join(",")}`] = { map: g.region, x: d.G.x, y: d.G.y + 1, dir: "down" };
  WARPS[`${centerMap}:${EXIT[centerMap].join(",")}`] = { map: g.region, x: d.C.x, y: d.C.y + 1, dir: "down" };
  const next = LATE[i + 1];
  GATES[g.region] = next ? { need: g.badge, warp: { map: next.region, x: 11, y: 27, dir: "up" } } : { need: g.badge, credits: true };
  NPCS[gymMap] = [{ x: 6, y: 2, color: "#d6d6d6", role: "gym", name: g.leader, badge: g.badge, intro: `gym${g.idx}Intro`, done: `gym${g.idx}Done`,
                    party: [{ species: g.mon, level: g.lv[0] }, { species: g.mon, level: g.lv[1] }] }];
  NPCS[centerMap] = [{ x: 6, y: 2, color: "#ff8fb0", role: "nurse", name: "Nurse" }, { x: 3, y: 2, color: "#8fd0ff", role: "pc", name: "Storage PC" }];
  NPCS[g.region] = [{ x: 16, y: 22, color: "#c0a060", role: "trainer", dialog: "ace", name: "Ace Trainer",
                      party: [{ species: g.mon, level: Math.max(2, g.lv[0] - 2) }], defeated: false }];
});

// Every gym in badge order (for objective text + tests).
export const GYM_BADGES = [
  { badge: 0, region: "town", gymMap: "gym", leader: "Leader Fern", town: "Willow Town" },
  { badge: 1, region: "north", gymMap: "gym2", leader: "Leader Marina", town: "Tidewater Town" },
  { badge: 2, region: "east", gymMap: "gym3", leader: "Leader Rocco", town: "Cinder Village" },
  ...LATE.map((g) => ({ badge: g.badge, region: g.region, gymMap: `gym${g.idx}`, leader: g.leader, town: g.town })),
];

const WALKABLE = new Set([".", "_", ":", "F", "H", "L", "G", "C", "V", "M", "D", "E"]);
export const tileAt = (map, x, y) => (x < 0 || y < 0 || y >= map.h || x >= map.w) ? "T" : map.grid[y][x];
export const npcAt = (mapName, x, y) => (NPCS[mapName] || []).find((n) => n.x === x && n.y === y) || null;
export const isBlocked = (mapName, x, y) => !WALKABLE.has(tileAt(MAPS[mapName], x, y)) || !!npcAt(mapName, x, y);
export const isEncounterTile = (mapName, ch) => ch === ":" || (mapName === "cave" && ch === "F");
