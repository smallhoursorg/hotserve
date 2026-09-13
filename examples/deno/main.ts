// A minimal hotserve app. On the box, liveswap starts it with the
// flags in hotserve.caddy and hands it a unix socket in $SOCKET: it
// never listens on a TCP port there. Locally, `deno task dev` has no
// $SOCKET, so it serves http://localhost:8000 instead.
import { join } from "@std/path";

const socket = Deno.env.get("SOCKET");
const version = Deno.env.get("APP_VERSION") ?? "dev";
const dataDir = Deno.env.get("DATA_DIR") ?? "./data";

// migrate.ts (pre_start) writes this before the new version starts;
// failing here, rather than on a request, fails the deploy's health
// gate while the old version is still serving.
const schema = JSON.parse(await Deno.readTextFile(join(dataDir, "schema.json")));

function handler(req: Request): Response {
  const { pathname } = new URL(req.url);
  if (pathname === "/health") {
    return new Response("ok\n");
  }
  if (pathname === "/") {
    return new Response(`hello from ${version} (schema v${schema.version})\n`);
  }
  return new Response("not found\n", { status: 404 });
}

const server = socket
  ? Deno.serve({ path: socket }, handler)
  : Deno.serve({ hostname: "127.0.0.1", port: 8000 }, handler);

// liveswap drains traffic away before it stops the old version, then
// sends SIGTERM (and SIGKILL after `grace`). Finish what is in flight
// and exit on the first one.
Deno.addSignalListener("SIGTERM", async () => {
  await server.shutdown();
  Deno.exit(0);
});
