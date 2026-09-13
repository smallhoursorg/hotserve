// The pre_start hook: runs in the new release, before it starts, with
// the same sandbox and environment as the app. A non-zero exit aborts
// the deploy and the old version keeps serving. Data belongs in
// $DATA_DIR (the app's shared/ dir on the box): release dirs are
// replaced on every deploy, shared/ survives them.
import { join } from "@std/path";

const dataDir = Deno.env.get("DATA_DIR") ?? "./data";
const file = join(dataDir, "schema.json");
const current = 1;

await Deno.mkdir(dataDir, { recursive: true });
let from = 0;
try {
  from = JSON.parse(await Deno.readTextFile(file)).version;
} catch (err) {
  if (!(err instanceof Deno.errors.NotFound)) throw err;
}
if (from < current) {
  await Deno.writeTextFile(file, JSON.stringify({ version: current }) + "\n");
  console.log(`migrated schema v${from} -> v${current}`);
} else {
  console.log(`schema v${from} is current`);
}
