// The two samples action.yml describes. No dependencies: node's own
// fs, os, http and child_process, so nothing is vendored or pinned.
// Runnable by hand on any Linux: `node main.js`, then `node post.js`
// with STATE_start set to the line main.js printed as `state=`.
'use strict';
const fs = require('fs');
const os = require('os');
const http = require('http');
const https = require('https');
const { execFileSync } = require('child_process');

const IMDS = 'http://169.254.169.254/metadata/instance/compute?api-version=2021-02-01';
const IPIFY = 'https://api.ipify.org';

// /proc/stat's cpu line, in clock ticks: user nice system idle iowait
// irq softirq steal guest guest_nice. Busy is user+nice+system+irq+
// softirq (guest time is inside user; steal is the hypervisor's, not
// the job's). Total adds idle, iowait and steal: the ticks that passed
// on every CPU, so busy/total is the utilisation with no clock, core
// count or HZ in it.
function ticks(line) {
  return line.trim().split(/\s+/).slice(1).map(Number);
}
function busy(t) {
  return t[0] + t[1] + t[2] + t[5] + t[6];
}
function total(t) {
  return busy(t) + t[3] + t[4] + t[7];
}
function delta(line0, line1) {
  const a = ticks(line0), b = ticks(line1);
  return { busy: busy(b) - busy(a), total: total(b) - total(a) };
}

function read(path) {
  try { return fs.readFileSync(path, 'utf8'); } catch { return ''; }
}
function run(cmd, args) {
  try { return execFileSync(cmd, args, { encoding: 'utf8', timeout: 2000 }).trim(); } catch { return ''; }
}
function statLine() {
  return read('/proc/stat').split('\n')[0];
}
function cgroup() {
  const m = read('/sys/fs/cgroup/cpu.stat').match(/^usage_usec (\d+)/m);
  return m ? m[1] : 'unknown';
}
// Resolves to the body on 200, to '' on anything else, within ms.
function get(url, ms, headers) {
  return new Promise(resolve => {
    const done = v => { clearTimeout(timer); resolve(v); };
    const timer = setTimeout(() => { req.destroy(); done(''); }, ms);
    const req = (url.startsWith('https:') ? https : http).get(url, { headers }, res => {
      let body = '';
      res.on('data', d => { body += d; });
      res.on('end', () => done(res.statusCode === 200 ? body : ''));
    });
    req.on('error', () => done(''));
  });
}

async function start() {
  // Probes first; the sample is the last thing, so none of their
  // waiting is inside the measured window.
  const [compute, ip] = await Promise.all([get(IMDS, 3000, { Metadata: 'true' }), get(IPIFY, 5000)]);
  let c = {};
  try { c = JSON.parse(compute); } catch { c = {}; }
  const model = (run('lscpu').match(/^Model name:\s*(.*\w.*)$/m) || [])[1] || (os.cpus()[0] || {}).model || 'unknown';
  const mem = (read('/proc/meminfo').match(/^MemTotal:\s+(\d+)/m) || [])[1] || 'unknown';
  console.log('::group::runner-sample start');
  console.log(`nproc=${os.cpus().length}`);
  console.log(`memtotal_kb=${mem}`);
  console.log(`arch=${os.arch()}`);
  console.log(`cpu_model=${model}`);
  // Only the three fields that place and size the machine: the full
  // document carries GitHub's subscription and resource ids.
  console.log(`azure_location=${c.location || 'no answer'}`);
  console.log(`azure_zone=${c.zone || ''}`);
  console.log(`azure_vmsize=${c.vmSize || 'no answer'}`);
  console.log(`egress_ip=${ip || 'no answer'}`);
  const line = statLine();
  const now = Date.now();
  console.log(`epoch_ms=${now}`);
  console.log(`stat=${line}`);
  console.log(`cgroup_usage_usec=${cgroup()}`);
  console.log('::endgroup::');
  const state = `${now} ${line}`;
  if (process.env.GITHUB_STATE) {
    fs.appendFileSync(process.env.GITHUB_STATE, `start=${state}\n`);
  } else {
    console.log(`state=${state}`);
  }
}

function end() {
  const line = statLine();
  const now = Date.now();
  console.log('::group::runner-sample end');
  console.log(`epoch_ms=${now}`);
  console.log(`stat=${line}`);
  console.log(`cgroup_usage_usec=${cgroup()}`);
  const saved = process.env.STATE_start || '';
  const sp = saved.indexOf(' ');
  if (sp > 0) {
    const d = delta(saved.slice(sp + 1), line);
    const hz = Number(run('getconf', ['CLK_TCK'])) || 100;
    console.log(`wall_s=${((now - Number(saved.slice(0, sp))) / 1000).toFixed(1)}`);
    console.log(`cpu_s=${(d.busy / hz).toFixed(1)}`);
    console.log(`utilisation=${d.total > 0 ? (d.busy / d.total).toFixed(3) : 'unknown'}`);
  } else {
    console.log('delta=no start sample');
  }
  console.log('::endgroup::');
}

// A post step that throws fails the job, so neither phase ever does.
function guarded(f) {
  Promise.resolve().then(f).catch(e => console.log(`runner-sample: ${e && e.message ? e.message : e}`));
}

module.exports = { delta, start, end, guarded };
