/* Minimal zero-dependency static server (ES modules need http, not file://).
 * Usage: node serve.mjs [port]   ->   http://localhost:8080 */
import http from "http";
import { readFile } from "fs/promises";
import path from "path";
import { fileURLToPath } from "url";

const root = path.dirname(fileURLToPath(import.meta.url));
const PORT = Number(process.argv[2]) || 8080;
const TYPES = {
  ".html": "text/html", ".js": "text/javascript", ".mjs": "text/javascript",
  ".css": "text/css", ".png": "image/png", ".json": "application/json",
};

export function createServer() {
  return http.createServer(async (req, res) => {
    try {
      let urlPath = decodeURIComponent(req.url.split("?")[0]);
      if (urlPath === "/") urlPath = "/index.html";
      const filePath = path.join(root, path.normalize(urlPath));
      if (!filePath.startsWith(root)) { res.writeHead(403); return res.end("Forbidden"); }
      const data = await readFile(filePath);
      res.writeHead(200, { "Content-Type": TYPES[path.extname(filePath)] || "application/octet-stream" });
      res.end(data);
    } catch {
      res.writeHead(404); res.end("Not found");
    }
  });
}

// Run directly (not when imported by the test harness).
if (process.argv[1] === fileURLToPath(import.meta.url)) {
  createServer().listen(PORT, () => console.log(`Shapemon dev server: http://localhost:${PORT}`));
}
