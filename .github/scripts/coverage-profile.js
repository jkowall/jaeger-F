// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

// Parses, merges, and scores Go coverage profiles without reading Go sources,
// so the coverage gate can run from a default-branch checkout against profiles
// produced by a pull request's code.
//
// Both ci-summary-report.yml (CLI) and ci-summary-report-publish.js (require)
// use this file, so the gating job and the published check run score a
// profile the same way.
//
// Profiles may come from a fork. Nothing here prints a file name or a line of
// profile content: callers in privileged workflows must not echo them.

'use strict';

const path = require('path');

const MODE_RE = /^mode: (set|count|atomic)$/;
const BLOCK_RE = /^(\S+\.go):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$/;

/**
 * Parse a Go coverage profile.
 * @param {string} text
 * @returns {{mode: string, blocks: Map<string, {stmts: number, count: number}>}|null}
 *   null when the text is not a well-formed profile
 */
function parseProfile(text) {
  if (typeof text !== 'string') return null;
  const lines = text.split('\n').map(line => line.replace(/\r$/, ''));
  const header = lines[0].match(MODE_RE);
  if (!header) return null;
  const blocks = new Map();
  for (const line of lines.slice(1)) {
    if (line === '') continue;
    const m = line.match(BLOCK_RE);
    if (!m) return null;
    const stmts = Number(m[6]);
    const count = Number(m[7]);
    if (!Number.isSafeInteger(stmts) || !Number.isSafeInteger(count)) return null;
    const key = `${m[1]}:${m[2]}.${m[3]},${m[4]}.${m[5]}`;
    const prev = blocks.get(key);
    if (prev && prev.stmts !== stmts) return null;
    blocks.set(key, { stmts, count: combine(header[1], prev ? prev.count : 0, count) });
  }
  return { mode: header[1], blocks };
}

function combine(mode, a, b) {
  if (mode === 'set') return a > 0 || b > 0 ? 1 : 0;
  return Math.min(a + b, Number.MAX_SAFE_INTEGER);
}

/**
 * Merge parsed profiles of one mode into a single profile text.
 * @param {Array<{mode: string, blocks: Map}>} profiles
 * @returns {string|null} null when the list is empty or the modes differ
 */
function mergeProfiles(profiles) {
  if (profiles.length === 0) return null;
  const mode = profiles[0].mode;
  if (profiles.some(p => p.mode !== mode)) return null;
  const merged = new Map();
  for (const p of profiles) {
    for (const [key, block] of p.blocks) {
      const prev = merged.get(key);
      if (prev && prev.stmts !== block.stmts) return null;
      merged.set(key, { stmts: block.stmts, count: combine(mode, prev ? prev.count : 0, block.count) });
    }
  }
  const lines = [`mode: ${mode}`];
  for (const [key, block] of merged) lines.push(`${key} ${block.stmts} ${block.count}`);
  return `${lines.join('\n')}\n`;
}

/**
 * Statement coverage of a profile, rounded to one decimal like `go tool cover -func`.
 * @param {string} text - profile contents
 * @returns {number|null} null when the profile is malformed or has no statements
 */
function coveragePercent(text) {
  const profile = parseProfile(text);
  if (!profile) return null;
  let total = 0;
  let covered = 0;
  for (const { stmts, count } of profile.blocks.values()) {
    total += stmts;
    if (count > 0) covered += stmts;
  }
  if (total === 0) return null;
  return Math.round((covered / total) * 1000) / 10;
}

/**
 * List the coverage profiles a CI run uploaded, as `coverage-<flag>/*.out`
 * under the artifacts directory (same layout check_coverage_uploads.py reads).
 * Symlinks are not followed.
 * @param {object} fs
 * @param {string} dir
 * @returns {string[]}
 */
function findProfiles(fs, dir) {
  const out = [];
  const visit = (base, depth) => {
    let entries;
    try {
      entries = fs.readdirSync(base, { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of entries.sort((a, b) => a.name.localeCompare(b.name))) {
      if (!entry.isDirectory()) continue;
      const sub = path.join(base, entry.name);
      if (entry.name.startsWith('coverage-')) {
        for (const file of fs.readdirSync(sub, { withFileTypes: true })) {
          if (file.isFile() && file.name.endsWith('.out')) out.push(path.join(sub, file.name));
        }
      } else if (depth === 0) {
        visit(sub, 1);
      }
    }
  };
  visit(dir, 0);
  return out;
}

function main(argv, fs) {
  const [command, ...args] = argv;
  if (command === 'merge' && args.length === 2) {
    const [dir, outFile] = args;
    fs.rmSync(outFile, { force: true });
    const files = findProfiles(fs, dir);
    if (files.length === 0) {
      console.log('::error::No coverage profiles found; the coverage gate cannot pass without them.');
      return 1;
    }
    const profiles = files.map(file => parseProfile(fs.readFileSync(file, 'utf8')));
    const bad = profiles.filter(p => p === null).length;
    if (bad > 0) {
      console.log(`::error::${bad} of ${files.length} coverage profile(s) are malformed.`);
      return 1;
    }
    const merged = mergeProfiles(profiles);
    if (merged === null) {
      console.log('::error::Coverage profiles disagree on mode or block sizes and cannot be merged.');
      return 1;
    }
    fs.writeFileSync(`${outFile}.tmp`, merged);
    fs.renameSync(`${outFile}.tmp`, outFile);
    console.log(`Merged ${files.length} coverage profile(s).`);
    return 0;
  }
  if (command === 'percent' && args.length === 1) {
    let text = null;
    try {
      text = fs.readFileSync(args[0], 'utf8');
    } catch {
      // reported below
    }
    const pct = coveragePercent(text);
    if (pct === null) {
      console.error('Coverage profile is missing, malformed, or has no statements.');
      return 1;
    }
    console.log(String(pct));
    return 0;
  }
  console.error('usage: coverage-profile.js merge <artifacts-dir> <out-file> | percent <profile>');
  return 2;
}

if (require.main === module) {
  process.exitCode = main(process.argv.slice(2), require('fs'));
}

module.exports = {
  parseProfile,
  mergeProfiles,
  coveragePercent,
  findProfiles,
  main,
};
