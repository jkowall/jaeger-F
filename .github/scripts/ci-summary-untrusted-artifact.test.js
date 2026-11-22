// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

'use strict';

// Spec for the CI summary trust boundary.
//
// A fork pull_request runs CI Orchestrator from the merge commit, so the
// summary job's checkout (ref: github.sha) contains the fork's Makefile,
// scripts, and local actions. That job writes ci-summary.json. The
// workflow_run publisher, which has checks: write and runs the default
// branch, turns that file into the Coverage Gate and Metrics Comparison
// check runs.
//
// These tests fail while that boundary is open. They pass once a missing
// profile, a profile below the gate, or a metrics snapshot with no diff
// cannot be reported as a successful check or as a successful summary job,
// no matter what the artifact or a replaced checkout script says.

const { spawnSync } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');

const handler = require('./ci-summary-report-publish');

const REPO = path.resolve(__dirname, '../..');
const REPORT_YAML = fs.readFileSync(
  path.join(REPO, '.github/workflows/ci-summary-report.yml'),
  'utf8',
);
const MINIMUM = '95.0';
const RUN_SHA = 'a'.repeat(40);

jest.setTimeout(180000);

let lowProfile = '';
let highProfile = '';

beforeAll(() => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cover-src-'));
  const out = path.join(dir, 'cover.out');
  const run = spawnSync('go', [
    'test', '-count=1', '-covermode=atomic', `-coverprofile=${out}`, './internal/version/',
  ], { cwd: REPO, encoding: 'utf8' });
  if (run.status !== 0) {
    throw new Error(`go test -coverprofile failed\n${run.stdout}\n${run.stderr}`);
  }
  lowProfile = fs.readFileSync(out, 'utf8');
  highProfile = rewriteCounts(lowProfile, () => 1);
  const lowPct = coverPercent(writeTempProfile(lowProfile));
  const highPct = coverPercent(writeTempProfile(highProfile));
  if (!(Number(lowPct) < 95)) {
    throw new Error(`fixture profile is not below the floor: ${lowPct}`);
  }
  if (!(Number(highPct) >= 95)) {
    throw new Error(`rewritten profile is not passing: ${highPct}`);
  }
});

function rewriteCounts(profile, countFor) {
  const lines = profile.split('\n').filter((line, i) => i === 0 || line.trim() !== '');
  const out = [lines[0]];
  for (const line of lines.slice(1)) {
    const match = line.match(/^(.*) (\d+) (\d+)$/);
    if (!match) throw new Error(`unrecognized cover line: ${line}`);
    out.push(`${match[1]} ${match[2]} ${countFor(match)}`);
  }
  return `${out.join('\n')}\n`;
}

function writeTempProfile(text) {
  const file = path.join(os.tmpdir(), `cover-${process.pid}-${Math.random().toString(16).slice(2)}.out`);
  fs.writeFileSync(file, text);
  return file;
}

function coverPercent(file) {
  const run = spawnSync('bash', ['-e', '-c',
    'go tool cover -func="$1" | grep "^total:" | awk \'{print $3}\' | tr -d \'%\'',
    'bash', file,
  ], { cwd: REPO, encoding: 'utf8' });
  if (run.status !== 0) {
    throw new Error(`go tool cover failed\n${run.stdout}\n${run.stderr}`);
  }
  return run.stdout.trim();
}

function bcLess(left, right) {
  const run = spawnSync('bash', ['-e', '-c', 'echo "$1 < $2" | bc -l', 'bash', String(left), String(right)], {
    encoding: 'utf8',
  });
  if (run.status !== 0) throw new Error(run.stderr);
  return run.stdout.trim() === '1';
}

// The gate rules in ci-summary-report.yml: absolute floor, then no regression
// against .artifacts/baseline-coverage.txt on a non-main ref.
function trustedCoverageConclusion(workspace) {
  const merged = path.join(workspace, '.artifacts/merged-coverage.out');
  if (!fs.existsSync(merged)) return 'failure';
  const pct = coverPercent(merged);
  if (!pct) return 'failure';
  if (bcLess(pct, MINIMUM)) return 'failure';
  const baselineFile = path.join(workspace, '.artifacts/baseline-coverage.txt');
  if (fs.existsSync(baselineFile)) {
    const baseline = fs.readFileSync(baselineFile, 'utf8').trim();
    if (!baseline || bcLess(pct, baseline)) return 'failure';
  }
  return 'success';
}

function trustedMetricsOutputs(workspace) {
  const output = path.join(workspace, '.metrics-output');
  fs.writeFileSync(output, '');
  const run = spawnSync('bash', ['./scripts/e2e/metrics_summary.sh'], {
    cwd: workspace,
    env: {
      ...process.env,
      GITHUB_OUTPUT: output,
      METRICS_DIR: path.join(workspace, '.artifacts'),
      GITHUB_REF: 'refs/pull/7/merge',
    },
    encoding: 'utf8',
  });
  if (run.status !== 0) {
    throw new Error(`metrics_summary.sh failed\n${run.stdout}\n${run.stderr}`);
  }
  return parseOutputFile(output);
}

function parseOutputFile(file) {
  const out = {};
  if (!fs.existsSync(file)) return out;
  for (const line of fs.readFileSync(file, 'utf8').split('\n')) {
    const eq = line.indexOf('=');
    if (eq > 0) out[line.slice(0, eq)] = line.slice(eq + 1);
  }
  return out;
}

function recordingCore() {
  const lines = [];
  const record = (line) => { lines.push(String(line)); };
  return { lines, info: jest.fn(record), warning: jest.fn(record), error: jest.fn(record) };
}

function recordingGithub() {
  return {
    paginate: jest.fn(async () => []),
    rest: {
      actions: { getWorkflowRun: jest.fn() },
      checks: {
        create: jest.fn(async () => ({ data: { id: 1, html_url: 'https://example.com/check/1' } })),
      },
      issues: {
        listComments: jest.fn(),
        createComment: jest.fn(async () => ({ data: { id: 2, html_url: 'https://example.com/comment/2' } })),
        updateComment: jest.fn(async () => ({ data: { html_url: 'https://example.com/comment/2' } })),
      },
      pulls: {
        list: jest.fn(async () => ({ data: [{ number: 42, head: { sha: RUN_SHA } }] })),
      },
    },
  };
}

function forkContext() {
  return {
    eventName: 'workflow_run',
    repo: { owner: 'org', repo: 'repo' },
    runId: 200,
    payload: {
      workflow_run: {
        id: 100,
        event: 'pull_request',
        head_sha: RUN_SHA,
        head_branch: 'feature',
        head_repository: { owner: { login: 'forker' } },
        pull_requests: [],
      },
    },
  };
}

function conclusionOf(github, name) {
  const calls = github.rest.checks.create.mock.calls
    .map((call) => call[0])
    .filter((arg) => arg.name === name);
  if (calls.length !== 1) {
    throw new Error(`${name} was created ${calls.length} times: ${JSON.stringify(calls)}`);
  }
  return calls[0];
}

function parseSteps(yaml) {
  const lines = yaml.split('\n');
  const start = lines.findIndex((line) => line === '  summary-report:');
  if (start < 0) throw new Error('summary-report job not found');
  let i = start + 1;
  while (i < lines.length && lines[i] !== '    steps:') {
    if (/^  \S/.test(lines[i])) throw new Error('summary-report has no steps');
    i += 1;
  }
  if (lines[i] !== '    steps:') throw new Error('steps key missing');
  i += 1;
  const steps = [];
  while (i < lines.length) {
    if (/^  \S/.test(lines[i])) break;
    if (!lines[i].startsWith('      - ')) {
      i += 1;
      continue;
    }
    const step = {
      name: '', id: '', uses: '', run: '', if: '', shell: '', env: {}, with: {},
    };
    const first = lines[i].slice('      - '.length);
    i += 1;
    const block = [first];
    while (i < lines.length && !lines[i].startsWith('      - ') && !/^  \S/.test(lines[i])) {
      block.push(lines[i]);
      i += 1;
    }
    assignStep(step, block);
    steps.push(step);
  }
  if (steps.length === 0) throw new Error('no steps parsed');
  return steps;
}

function assignStep(step, block) {
  let mode = null;
  let buf = [];
  const commit = () => {
    if (!mode) return;
    const text = buf.join('\n').replace(/\n$/, '');
    if (mode === 'run' || mode === 'if') step[mode] = text;
    else step[mode] = parseMap(text);
    mode = null;
    buf = [];
  };
  const take = (line) => {
    const match = line.match(/^(\s*)([A-Za-z0-9_-]+):\s*(.*)$/);
    if (!match || (mode && match[1].length > 8)) return false;
    commit();
    const key = match[2];
    const value = match[3];
    if (value === '|' || value === '|-' || value === '>' || value === '>-') {
      if (key === 'run' || key === 'if' || key === 'env' || key === 'with') mode = key;
      return true;
    }
    if (value === '' && (key === 'env' || key === 'with')) {
      mode = key;
      return true;
    }
    if (key === 'name') step.name = unquote(value);
    else if (key === 'id') step.id = value;
    else if (key === 'uses') step.uses = value;
    else if (key === 'shell') step.shell = value;
    else if (key === 'run') step.run = value;
    else if (key === 'if') step.if = value;
    return true;
  };
  for (const line of block) {
    if (mode && (/^ {8}\S/.test(line) || /^ {0,8}\S/.test(line))) commit();
    if (mode) {
      buf.push(line.replace(/^ {10}/, ''));
      continue;
    }
    take(line.trimEnd());
  }
  commit();
}

function unquote(value) {
  return value.replace(/^['"]|['"]$/g, '');
}

function parseMap(text) {
  const map = {};
  for (const line of text.split('\n')) {
    const match = line.match(/^\s*([A-Za-z0-9_]+):\s*(.*)$/);
    if (match) map[match[1]] = match[2].trim();
  }
  return map;
}

function checksOutUntrustedCode(steps) {
  const checkout = steps.find((step) => step.uses.startsWith('actions/checkout'));
  if (!checkout) return false;
  const ref = checkout.with.ref || '';
  // pull_request sets github.sha and github.ref to the merge commit, which
  // contains the fork's tree. A base-branch ref does not.
  if (ref === '') return true;
  if (/github\.sha|github\.ref|head\.sha|head\.ref|refs\/pull\//.test(ref)) return true;
  return false;
}

function linkRepo(dir) {
  for (const name of fs.readdirSync(REPO)) {
    if (name === '.git' || name === '.artifacts' || name === '.tools') continue;
    fs.symlinkSync(path.join(REPO, name), path.join(dir, name));
  }
  fs.mkdirSync(path.join(dir, '.artifacts'));
  fs.mkdirSync(path.join(dir, '.tools'));
  const gopath = spawnSync('go', ['env', 'GOPATH'], { encoding: 'utf8' }).stdout.trim();
  const bin = path.join(gopath, 'bin', 'gocovmerge');
  if (!fs.existsSync(bin)) throw new Error(`gocovmerge is not installed at ${bin}`);
  fs.copyFileSync(bin, path.join(dir, '.tools', 'gocovmerge'));
  fs.chmodSync(path.join(dir, '.tools', 'gocovmerge'), 0o755);
  // The real Makefile resolves SRC_ROOT via git rev-parse, which walks out of
  // this temp dir to the checkout. The stub only reproduces the postcondition
  // of a successful install: .tools/gocovmerge is already the real binary.
  fs.unlinkSync(path.join(dir, 'Makefile'));
  fs.writeFileSync(path.join(dir, 'Makefile'), 'install-coverage-tools:\n\t@test -x .tools/gocovmerge\n');
}

function realize(dirPath) {
  if (!fs.lstatSync(dirPath).isSymbolicLink()) return;
  const real = fs.realpathSync(dirPath);
  fs.unlinkSync(dirPath);
  fs.mkdirSync(dirPath);
  for (const name of fs.readdirSync(real)) {
    fs.symlinkSync(path.join(real, name), path.join(dirPath, name));
  }
}

function writeOver(dir, rel, contents, mode) {
  const parts = rel.split('/');
  let cur = dir;
  for (const part of parts.slice(0, -1)) {
    cur = path.join(cur, part);
    realize(cur);
  }
  const full = path.join(dir, rel);
  if (fs.lstatSync(full).isSymbolicLink()) fs.unlinkSync(full);
  fs.writeFileSync(full, contents, { mode: mode || 0o644 });
}

const ATTACKER_METRICS = `#!/bin/bash
echo "INFRA_ERRORS=false" >> "$GITHUB_OUTPUT"
echo "TOTAL_CHANGES=0" >> "$GITHUB_OUTPUT"
echo "CONCLUSION=success" >> "$GITHUB_OUTPUT"
`;

function attackerFilter(realSource) {
  const main = realSource.indexOf('\ndef main(');
  if (main < 0) throw new Error('filter_coverage.py has no main');
  return `${realSource.slice(0, main)}
def main():
    path = sys.argv[1]
    lines = open(path).read().splitlines()
    out = []
    for i, line in enumerate(lines):
        if i == 0 or not line.strip():
            out.append(line)
            continue
        head, _count = line.rsplit(' ', 1)
        out.append(head + ' 1')
    open(path, 'w').write('\\n'.join(out) + '\\n')

if __name__ == '__main__':
    main()
`;
}

function attackerMakefile(highFile) {
  return `install-coverage-tools:
\tprintf '%s\\n' '#!/bin/sh' 'cat ${highFile}' > .tools/gocovmerge
\tchmod +x .tools/gocovmerge
`;
}

function seedCoverage(workspace, profile) {
  const dir = path.join(workspace, '.artifacts/coverage-unittests');
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, 'cover.out'), profile);
}

function seedCleanMetrics(workspace) {
  const snap = path.join(workspace, '.artifacts/metrics_snapshot_cass');
  const diff = path.join(workspace, '.artifacts/diff_metrics_snapshot_cass');
  fs.mkdirSync(snap, { recursive: true });
  fs.mkdirSync(diff, { recursive: true });
  fs.writeFileSync(path.join(snap, 'metrics_snapshot_cass.txt'), 'metric 1\n');
  fs.writeFileSync(path.join(diff, 'diff_metrics_snapshot_cass.txt'), '');
}

function seedMissingDiff(workspace) {
  const snap = path.join(workspace, '.artifacts/metrics_snapshot_cass');
  fs.mkdirSync(snap, { recursive: true });
  fs.writeFileSync(path.join(snap, 'metrics_snapshot_cass.txt'), 'metric 1\n');
}

function makeWorkspace(attack) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'summary-job-'));
  linkRepo(dir);
  const steps = parseSteps(fs.readFileSync(path.join(REPO, '.github/workflows/ci-summary-report.yml'), 'utf8'));
  const untrusted = checksOutUntrustedCode(steps);
  if (untrusted && attack === 'metrics-script') {
    writeOver(dir, 'scripts/e2e/metrics_summary.sh', ATTACKER_METRICS, 0o755);
  }
  if (untrusted && attack === 'filter') {
    const real = fs.readFileSync(path.join(REPO, 'scripts/e2e/filter_coverage.py'), 'utf8');
    writeOver(dir, 'scripts/e2e/filter_coverage.py', attackerFilter(real));
  }
  if (untrusted && attack === 'makefile') {
    const high = path.join(dir, 'forced-high.out');
    fs.writeFileSync(high, highProfile);
    fs.writeFileSync(path.join(dir, 'Makefile'), attackerMakefile(high));
  }
  return dir;
}

function previousFailed(state) {
  return Object.values(state).some((step) => step.outcome === 'failure' || step.outcome === 'cancelled');
}

function resolveRef(expr, state) {
  const output = expr.match(/^steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)$/);
  if (output) return (state[output[1]] && state[output[1]].outputs[output[2]]) || '';
  const outcome = expr.match(/^steps\.([A-Za-z0-9_-]+)\.outcome$/);
  if (outcome) return (state[outcome[1]] && state[outcome[1]].outcome) || '';
  return '';
}

function evalIf(raw, state) {
  let expr = (raw || '').trim();
  const hasStatus = /\b(success|failure|always|cancelled)\s*\(/.test(expr);
  if (!hasStatus) expr = expr ? `success() && (${expr})` : 'success()';
  const tokens = expr.match(/success\(\)|failure\(\)|always\(\)|cancelled\(\)|'[^']*'|&&|\|\||==|!=|\(|\)|[A-Za-z_][A-Za-z0-9_.-]*/g);
  if (!tokens) throw new Error(`cannot tokenize if: ${expr}`);
  let pos = 0;
  const peek = () => tokens[pos];
  const eat = (value) => {
    if (tokens[pos] !== value) throw new Error(`expected ${value} in ${expr}`);
    pos += 1;
  };
  function or() {
    let left = and();
    while (peek() === '||') {
      pos += 1;
      const right = and();
      left = left || right;
    }
    return left;
  }
  function and() {
    let left = cmp();
    while (peek() === '&&') {
      pos += 1;
      const right = cmp();
      left = left && right;
    }
    return left;
  }
  function cmp() {
    const left = primary();
    if (peek() === '==' || peek() === '!=') {
      const op = tokens[pos];
      pos += 1;
      const right = primary();
      const same = String(left).toLowerCase() === String(right).toLowerCase();
      return op === '==' ? same : !same;
    }
    return left;
  }
  function primary() {
    const tok = peek();
    if (tok === '(') { pos += 1; const value = or(); eat(')'); return value; }
    pos += 1;
    if (tok === 'success()') return !previousFailed(state);
    if (tok === 'failure()') return previousFailed(state);
    if (tok === 'always()') return true;
    if (tok === 'cancelled()') return false;
    if (tok.startsWith("'")) return tok.slice(1, -1);
    if (tok === 'github.ref') return 'refs/pull/7/merge';
    if (tok.startsWith('steps.')) return resolveRef(tok, state);
    if (/^[A-Za-z_]/.test(tok)) return '';
    throw new Error(`bad token ${tok}`);
  }
  const value = or();
  if (pos !== tokens.length) throw new Error(`unparsed if tail: ${tokens.slice(pos).join(' ')}`);
  return value === true;
}

function substituteExpressions(text, state) {
  return text.replace(/\$\{\{\s*([^}]+?)\s*\}\}/g, (_all, expr) => resolveRef(expr.trim(), state));
}

function runJob(workspace) {
  const steps = parseSteps(fs.readFileSync(path.join(REPO, '.github/workflows/ci-summary-report.yml'), 'utf8'));
  const state = {};
  const log = [];
  let failed = false;
  for (const step of steps) {
    const id = step.id || step.name;
    if (!evalIf(step.if, state)) {
      state[id] = { outcome: 'skipped', outputs: {} };
      log.push(`SKIP ${step.name}`);
      continue;
    }
    const harnessed = step.uses || /pip install|gh run download/.test(step.run);
    if (harnessed) {
      // Checkout, cache, and artifact download are GitHub actions. The files
      // they would have produced are already in the workspace. pip installs
      // packages none of the gate scripts import.
      state[id] = { outcome: 'success', outputs: {} };
      log.push(`HARNESS ${step.name}`);
      continue;
    }
    const script = substituteExpressions(step.run, state);
    const scriptFile = path.join(workspace, `.step-${log.length}.sh`);
    const outputFile = path.join(workspace, `.out-${log.length}`);
    fs.writeFileSync(scriptFile, script);
    fs.writeFileSync(outputFile, '');
    const args = step.shell === 'bash'
      ? ['--noprofile', '--norc', '-eo', 'pipefail', scriptFile]
      : ['-e', scriptFile];
    const env = { ...process.env, GITHUB_OUTPUT: outputFile, GITHUB_REF: 'refs/pull/7/merge' };
    for (const [key, value] of Object.entries(step.env)) {
      env[key] = substituteExpressions(value, state);
    }
    const run = spawnSync('bash', args, { cwd: workspace, env, encoding: 'utf8' });
    const outputs = parseOutputFile(outputFile);
    const outcome = run.status === 0 ? 'success' : 'failure';
    state[id] = { outcome, outputs };
    if (outcome === 'failure') failed = true;
    log.push([
      `${outcome.toUpperCase()} ${step.name} exit=${run.status}`,
      `outputs=${JSON.stringify(outputs)}`,
      (run.stdout || '').split('\n').slice(-8).join('\n'),
      (run.stderr || '').split('\n').slice(-8).join('\n'),
    ].filter(Boolean).join('\n'));
  }
  return { failed, log: log.join('\n---\n'), state };
}

function saveArtifact(workspace, env) {
  const steps = parseSteps(REPORT_YAML);
  const save = steps.find((step) => step.name === 'Save conclusions for publish workflow');
  if (!save) throw new Error('save step missing');
  const scriptFile = path.join(workspace, '.save.sh');
  fs.writeFileSync(scriptFile, save.run);
  const run = spawnSync('bash', ['-e', scriptFile], {
    cwd: workspace,
    env: { ...process.env, ...env },
    encoding: 'utf8',
  });
  if (run.status !== 0) {
    throw new Error(`save step failed\n${run.stdout}\n${run.stderr}`);
  }
}

async function publish(workspace) {
  const prev = process.cwd();
  process.chdir(workspace);
  const github = recordingGithub();
  try {
    await handler({ github, core: recordingCore(), context: forkContext(), fs });
  } finally {
    process.chdir(prev);
  }
  return {
    coverage: conclusionOf(github, 'Coverage Gate'),
    metrics: conclusionOf(github, 'Metrics Comparison'),
  };
}

describe('publish workflow check runs from an untrusted summary artifact', () => {
  test('a skipped coverage result with no profile is not a successful Coverage Gate', async () => {
    const workspace = makeWorkspace(null);
    seedCleanMetrics(workspace);
    saveArtifact(workspace, {
      METRICS_CONCLUSION: 'success',
      METRICS_TOTAL: '0',
      METRICS_INFRA_ERRORS: 'false',
      COVERAGE_MERGE_OUTCOME: 'success',
      COVERAGE_SKIPPED: 'true',
      COVERAGE_CONCLUSION: '',
      COVERAGE_PCT: '',
    });
    const posted = await publish(workspace);
    expect(trustedCoverageConclusion(workspace)).toBe('failure');
    expect(posted.coverage).toMatchObject({ status: 'completed', conclusion: 'failure' });
    expect(posted.metrics).toMatchObject({ status: 'completed', conclusion: 'success' });
  });

  test('an artifact that reports coverage success cannot hide a profile under 95 percent', async () => {
    const workspace = makeWorkspace(null);
    seedCleanMetrics(workspace);
    fs.writeFileSync(path.join(workspace, '.artifacts/merged-coverage.out'), lowProfile);
    saveArtifact(workspace, {
      METRICS_CONCLUSION: 'success',
      METRICS_TOTAL: '0',
      METRICS_INFRA_ERRORS: 'false',
      COVERAGE_MERGE_OUTCOME: 'success',
      COVERAGE_SKIPPED: 'false',
      COVERAGE_CONCLUSION: 'success',
      COVERAGE_PCT: '99.9',
    });
    const posted = await publish(workspace);
    expect(trustedCoverageConclusion(workspace)).toBe('failure');
    expect(posted.coverage.conclusion).toBe('failure');
    expect(posted.metrics.conclusion).toBe('success');
  });

  test('an artifact that reports coverage success cannot hide a regression against the baseline file', async () => {
    const workspace = makeWorkspace(null);
    seedCleanMetrics(workspace);
    fs.writeFileSync(path.join(workspace, '.artifacts/merged-coverage.out'), highProfile);
    fs.writeFileSync(path.join(workspace, '.artifacts/baseline-coverage.txt'), '100.1\n');
    saveArtifact(workspace, {
      METRICS_CONCLUSION: 'success',
      METRICS_TOTAL: '0',
      METRICS_INFRA_ERRORS: 'false',
      COVERAGE_MERGE_OUTCOME: 'success',
      COVERAGE_SKIPPED: 'false',
      COVERAGE_CONCLUSION: 'success',
      COVERAGE_PCT: '100',
    });
    const posted = await publish(workspace);
    expect(trustedCoverageConclusion(workspace)).toBe('failure');
    expect(posted.coverage.conclusion).toBe('failure');
  });

  test('a passing profile is a successful Coverage Gate when the artifact reports failure', async () => {
    const workspace = makeWorkspace(null);
    seedCleanMetrics(workspace);
    fs.writeFileSync(path.join(workspace, '.artifacts/merged-coverage.out'), highProfile);
    saveArtifact(workspace, {
      METRICS_CONCLUSION: 'failure',
      METRICS_TOTAL: '',
      METRICS_INFRA_ERRORS: 'true',
      COVERAGE_MERGE_OUTCOME: 'failure',
      COVERAGE_SKIPPED: 'false',
      COVERAGE_CONCLUSION: 'failure',
      COVERAGE_PCT: '1',
    });
    const posted = await publish(workspace);
    expect(trustedCoverageConclusion(workspace)).toBe('success');
    expect(trustedMetricsOutputs(workspace).CONCLUSION).toBe('success');
    expect(posted.coverage.conclusion).toBe('success');
    expect(posted.metrics.conclusion).toBe('success');
  });

  test('a numeric metrics total cannot hide a snapshot that has no diff', async () => {
    const workspace = makeWorkspace(null);
    seedMissingDiff(workspace);
    fs.writeFileSync(path.join(workspace, '.artifacts/merged-coverage.out'), highProfile);
    saveArtifact(workspace, {
      METRICS_CONCLUSION: 'success',
      METRICS_TOTAL: '0',
      METRICS_INFRA_ERRORS: 'false',
      COVERAGE_MERGE_OUTCOME: 'success',
      COVERAGE_SKIPPED: 'false',
      COVERAGE_CONCLUSION: 'success',
      COVERAGE_PCT: '100',
    });
    const posted = await publish(workspace);
    expect(trustedMetricsOutputs(workspace).CONCLUSION).toBe('failure');
    expect(posted.metrics).toMatchObject({ status: 'completed', conclusion: 'failure' });
    expect(posted.coverage.conclusion).toBe('success');
  });
});

describe('summary job verdict from the pull request checkout', () => {
  test('the job fails when no coverage profile was uploaded', () => {
    const workspace = makeWorkspace(null);
    seedCleanMetrics(workspace);
    const result = runJob(workspace);
    expect({ jobFailed: result.failed, log: result.log }).toMatchObject({ jobFailed: true });
  });

  test('the job fails when the checkout metrics script reports success for a missing diff', () => {
    const workspace = makeWorkspace('metrics-script');
    seedMissingDiff(workspace);
    seedCoverage(workspace, highProfile);
    const result = runJob(workspace);
    expect({ jobFailed: result.failed, log: result.log }).toMatchObject({ jobFailed: true });
  });

  test('the job fails when the checkout filter script rewrites a low profile to 100 percent', () => {
    const workspace = makeWorkspace('filter');
    seedCleanMetrics(workspace);
    seedCoverage(workspace, lowProfile);
    const result = runJob(workspace);
    expect({ jobFailed: result.failed, log: result.log }).toMatchObject({ jobFailed: true });
  });

  test('the job fails when the checkout Makefile replaces gocovmerge with one that emits 100 percent', () => {
    const workspace = makeWorkspace('makefile');
    seedCleanMetrics(workspace);
    seedCoverage(workspace, lowProfile);
    const result = runJob(workspace);
    expect({ jobFailed: result.failed, log: result.log }).toMatchObject({ jobFailed: true });
  });
});
