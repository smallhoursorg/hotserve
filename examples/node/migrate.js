// The pre_start hook: runs in the new release, before it starts, with
// the same sandbox and environment as the app. A non-zero exit aborts
// the deploy and the old version keeps serving. Data belongs in
// $DATA_DIR (the app's shared/ dir on the box): release dirs are
// replaced on every deploy, shared/ survives them.
const fs = require("node:fs");
const path = require("node:path");

const dataDir = process.env.DATA_DIR ?? "./data";
const file = path.join(dataDir, "schema.json");
const current = 1;

fs.mkdirSync(dataDir, { recursive: true });
let from = 0;
try {
  from = JSON.parse(fs.readFileSync(file, "utf8")).version;
} catch (err) {
  if (err.code !== "ENOENT") throw err;
}
if (from < current) {
  fs.writeFileSync(file, JSON.stringify({ version: current }) + "\n");
  console.log(`migrated schema v${from} -> v${current}`);
} else {
  console.log(`schema v${from} is current`);
}
