// A minimal hotserve app, built into one executable by scripts/bundle.sh
// (Node's single-executable build), so the box needs no Node installed.
// On the box, liveswap starts it and hands it a unix socket in $SOCKET:
// it never listens on a TCP port there. Locally, `npm run dev` has no
// $SOCKET, so it serves http://127.0.0.1:8000 instead.
const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");

const socket = process.env.SOCKET;
const version = process.env.APP_VERSION ?? "dev";
const dataDir = process.env.DATA_DIR ?? "./data";

// migrate.js (pre_start) writes this before the new version starts;
// failing here, rather than on a request, fails the deploy's health
// gate while the old version is still serving.
const schema = JSON.parse(fs.readFileSync(path.join(dataDir, "schema.json"), "utf8"));

const server = http.createServer((req, res) => {
  const { pathname } = new URL(req.url, "http://localhost");
  if (pathname === "/health") return res.end("ok\n");
  if (pathname === "/") return res.end(`hello from ${version} (schema v${schema.version})\n`);
  res.statusCode = 404;
  res.end("not found\n");
});

if (socket) server.listen(socket);
else server.listen(8000, "127.0.0.1", () => console.log("http://127.0.0.1:8000"));

// liveswap drains traffic away before it stops the old version, then
// sends SIGTERM (and SIGKILL after `grace`). Finish what is in flight
// and exit on the first one.
process.on("SIGTERM", () => server.close(() => process.exit(0)));
