#!/usr/bin/env node
// The file length ratchet. No file may be longer than its cap, except the ones recorded in
// line-cap-baseline.json, and those may shrink but never grow.
//
//   node scripts/line-cap.mjs                  check the tree against the baseline
//   node scripts/line-cap.mjs --base <commit>  also refuse baseline numbers raised since <commit>
//   node scripts/line-cap.mjs --update         lower the baseline after shortening files
//   node scripts/line-cap.mjs --seed           write the first baseline (refused when one exists)
//
// The same script is used in every TechBeaver repo. What differs per repo is
// line-cap.config.json, which says where to look. The caps are constants here on purpose:
// loosening one is a change to this file that a reviewer sees, never a quiet edit of a number
// in a JSON file next to a hundred others.
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const LIMITS = Object.freeze({ source: 500, test: 1000, style: 1000 });
export const BASELINE_FILE = "line-cap-baseline.json";
export const CONFIG_FILE = "line-cap.config.json";
const MIN_REASON = 20;
const STYLE = /\.(css|scss|sass|less)$/;
const TEST = /(_test\.go|[._](test|spec)\.[cm]?[jt]sx?)$|(^|\/)(__tests__|__mocks__)\//;
const GENERATED = /Code generated .*DO NOT EDIT|@generated/;

class Problem extends Error {}

/** Which cap a path is held to. */
export function kindOf(path) {
  if (STYLE.test(path)) return "style";
  if (TEST.test(path)) return "test";
  return "source";
}

/** Matches `wc -l`, plus one for a last line with no newline. */
export function countLines(text) {
  if (text.length === 0) return 0;
  let n = 0;
  for (let i = text.indexOf("\n"); i !== -1; i = text.indexOf("\n", i + 1)) n++;
  return text.endsWith("\n") ? n : n + 1;
}

function isGenerated(text) {
  return GENERATED.test(text.split("\n", 6).join("\n"));
}

export function parseConfig(text) {
  let raw;
  try {
    raw = JSON.parse(text);
  } catch (e) {
    throw new Problem(`${CONFIG_FILE} is not valid JSON: ${e.message}`);
  }
  const lists = ["include", "extensions", "exclude"];
  for (const key of Object.keys(raw)) {
    if (!lists.includes(key)) throw new Problem(`${CONFIG_FILE} has unknown key "${key}"`);
  }
  for (const key of lists) {
    const v = raw[key] ?? [];
    if (!Array.isArray(v) || v.some((s) => typeof s !== "string" || s === "")) {
      throw new Problem(`${CONFIG_FILE}: "${key}" must be a list of non-empty strings`);
    }
    raw[key] = v;
  }
  if (raw.include.length === 0 || raw.extensions.length === 0) {
    throw new Problem(`${CONFIG_FILE}: "include" and "extensions" must each name at least one entry, or nothing is checked`);
  }
  return raw;
}

function covered(path, config) {
  const under = (prefix) => path === prefix || path.startsWith(prefix.endsWith("/") ? prefix : prefix + "/");
  return (
    config.extensions.some((ext) => path.endsWith(ext)) &&
    config.include.some(under) &&
    !config.exclude.some(under)
  );
}

/** Every tracked, covered, hand-written file over its cap, as { path: lines }. */
export function measure(root, config) {
  let listing;
  try {
    listing = execFileSync("git", ["-C", root, "ls-files", "-z"], { encoding: "utf8", maxBuffer: 1 << 28 });
  } catch (e) {
    throw new Problem(`cannot list tracked files in ${root}: ${firstLine(e)}`);
  }
  const over = {};
  for (const path of listing.split("\0")) {
    if (path === "" || !covered(path, config)) continue;
    const file = join(root, path);
    if (!existsSync(file)) continue; // deleted in the working tree, not yet committed
    const text = readFileSync(file, "utf8");
    if (isGenerated(text)) continue;
    const lines = countLines(text);
    if (lines > LIMITS[kindOf(path)]) over[path] = lines;
  }
  return over;
}

export function newBaseline(allowed = {}, exceptions = []) {
  return { version: 1, limits: { ...LIMITS }, allowed, exceptions };
}

/**
 * Reads a baseline and rejects anything it does not fully understand. With strict off it is
 * reading an older commit's file, whose caps may differ from today's.
 */
export function parseBaseline(text, { strict = true } = {}) {
  let raw;
  try {
    raw = JSON.parse(text);
  } catch (e) {
    throw new Problem(`${BASELINE_FILE} is not valid JSON: ${e.message}`);
  }
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) throw new Problem(`${BASELINE_FILE} must be an object`);
  for (const key of Object.keys(raw)) {
    if (!["version", "limits", "allowed", "exceptions"].includes(key)) throw new Problem(`${BASELINE_FILE} has unknown key "${key}"`);
  }
  if (raw.version !== 1) throw new Problem(`${BASELINE_FILE} version ${raw.version} is not supported`);
  const limits = raw.limits ?? {};
  for (const kind of Object.keys(LIMITS)) {
    if (!Number.isInteger(limits[kind])) throw new Problem(`${BASELINE_FILE} does not record the ${kind} cap`);
    if (strict && limits[kind] !== LIMITS[kind]) {
      throw new Problem(`${BASELINE_FILE} claims a ${kind} cap of ${limits[kind]} and this tool enforces ${LIMITS[kind]}. The caps live in scripts/line-cap.mjs and changing one is the owner's decision.`);
    }
  }
  const allowed = raw.allowed ?? {};
  for (const [path, v] of Object.entries(allowed)) {
    if (!Number.isInteger(v)) throw new Problem(`${BASELINE_FILE}: ${path} must be a whole number`);
    if (strict && v <= LIMITS[kindOf(path)]) throw new Problem(`${BASELINE_FILE}: ${path} = ${v} is not above the cap, remove the entry`);
  }
  const exceptions = raw.exceptions ?? [];
  if (!Array.isArray(exceptions)) throw new Problem(`${BASELINE_FILE}: "exceptions" must be a list`);
  return { version: 1, limits, allowed, exceptions };
}

export function serialise(baseline) {
  const allowed = Object.fromEntries(Object.entries(baseline.allowed).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
  return JSON.stringify({ ...baseline, allowed }, null, 2) + "\n";
}

/** Files longer than they are allowed to be, and how many entries could now be lowered. */
export function compare(measured, baseline) {
  const violations = [];
  for (const [path, lines] of Object.entries(measured)) {
    const allowed = baseline.allowed[path] ?? LIMITS[kindOf(path)];
    if (lines > allowed) violations.push({ path, lines, allowed });
  }
  violations.sort((a, b) => (a.path < b.path ? -1 : 1));
  const slack = Object.entries(baseline.allowed).filter(([path, allowed]) => (measured[path] ?? 0) < allowed).length;
  return { violations, slack };
}

/** Lowers the baseline to the measurement. Never raises a number and never adds an entry. */
export function tighten(measured, baseline) {
  const allowed = {};
  for (const [path, was] of Object.entries(baseline.allowed)) {
    const now = measured[path];
    if (now === undefined) continue; // at or under the cap now, or gone: the debt is paid
    allowed[path] = Math.min(now, was);
  }
  return newBaseline(allowed, baseline.exceptions);
}

function exceptionProblem(exceptions, path, to) {
  const e = exceptions.find((x) => x && x.key === path && x.to === to);
  if (!e) return "no exception entry names this file and new value";
  if (typeof e.reason !== "string" || e.reason.trim().length < MIN_REASON) return `its exception needs a reason of at least ${MIN_REASON} characters`;
  if (typeof e.approved_by !== "string" || e.approved_by.trim() === "") return "its exception has no approved_by";
  if (!/^\d{4}-\d{2}-\d{2}$/.test(e.date ?? "")) return "its exception needs a date as YYYY-MM-DD";
  return "";
}

/** Baseline numbers that went up since an older baseline with no complete exception. */
export function unexplainedRaises(current, old) {
  const raises = [];
  for (const [path, to] of Object.entries(current.allowed)) {
    const from = old.allowed[path] ?? old.limits[kindOf(path)];
    if (to <= from) continue;
    const why = exceptionProblem(current.exceptions, path, to);
    if (why) raises.push({ path, from, to, why });
  }
  return raises.sort((a, b) => (a.path < b.path ? -1 : 1));
}

function firstLine(e) {
  return String(e.stderr || e.message || e).trim().split("\n")[0];
}

/** The baseline as it was at a commit, or null when that commit has none. */
export function baselineAt(root, ref) {
  try {
    execFileSync("git", ["-C", root, "rev-parse", "--verify", "--quiet", `${ref}^{commit}`], { stdio: "pipe" });
  } catch (e) {
    throw new Problem(`cannot find commit ${ref} to compare the baseline with: ${firstLine(e) || "unknown revision"}`);
  }
  try {
    execFileSync("git", ["-C", root, "cat-file", "-e", `${ref}:${BASELINE_FILE}`], { stdio: "pipe" });
  } catch {
    return null; // the commit before the ratchet existed
  }
  return parseBaseline(execFileSync("git", ["-C", root, "show", `${ref}:${BASELINE_FILE}`], { encoding: "utf8" }), { strict: false });
}

function save(path, baseline) {
  try {
    writeFileSync(path, serialise(baseline));
  } catch (e) {
    throw new Problem(`cannot write ${BASELINE_FILE}: ${e.message}`);
  }
}

function parseArgs(argv) {
  const opts = { root: ".", base: "", update: false, seed: false };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--update") opts.update = true;
    else if (a === "--seed") opts.seed = true;
    else if ((a === "--root" || a === "--base") && argv[i + 1] !== undefined) opts[a.slice(2)] = argv[++i];
    else throw new Problem(`unexpected argument ${a}`);
  }
  if (opts.update && opts.seed) throw new Problem("--update and --seed cannot be used together");
  return opts;
}

function summary(measured) {
  const count = { source: 0, test: 0, style: 0 };
  for (const path of Object.keys(measured)) count[kindOf(path)]++;
  return `Remaining debt: ${count.source} source files over ${LIMITS.source} lines, ${count.test} test files over ${LIMITS.test}, ${count.style} stylesheets over ${LIMITS.style}.`;
}

/** Runs the tool. Returns the exit code: 0 pass, 1 the ratchet is broken, 2 it could not run. */
export function run(argv, out = console.log, err = console.error) {
  try {
    return runChecked(parseArgs(argv), out);
  } catch (e) {
    if (!(e instanceof Problem)) throw e;
    err(`line-cap: ${e.message}`);
    return 2;
  }
}

function runChecked(opts, out) {
  const root = resolve(opts.root);
  const configPath = join(root, CONFIG_FILE);
  if (!existsSync(configPath)) throw new Problem(`${CONFIG_FILE} is missing, so there is nothing to say which files to check`);
  const measured = measure(root, parseConfig(readFileSync(configPath, "utf8")));
  const path = join(root, BASELINE_FILE);

  if (opts.seed) {
    if (existsSync(path)) throw new Problem(`${BASELINE_FILE} already exists. --seed is only for a repo with no baseline.`);
    save(path, newBaseline(measured));
    out(`wrote ${BASELINE_FILE}. ${summary(measured)}`);
    return 0;
  }
  if (!existsSync(path)) throw new Problem(`${BASELINE_FILE} is missing. It must be committed; do not regenerate it to get a pass.`);
  const baseline = parseBaseline(readFileSync(path, "utf8"));

  if (opts.update) save(path, tighten(measured, baseline));

  let failed = false;
  if (opts.base && !opts.update) {
    const old = baselineAt(root, opts.base);
    const raises = old ? unexplainedRaises(baseline, old) : [];
    if (raises.length > 0) {
      failed = true;
      out(`${BASELINE_FILE} was raised since ${opts.base.slice(0, 12)}:\n`);
      for (const r of raises) out(`  ${r.path}: ${r.from} -> ${r.to} (${r.why})`);
      out(`\nThe baseline only goes down. A raise needs the owner's explicit yes and an "exceptions" entry.\n`);
    }
  }
  const { violations, slack } = compare(measured, baseline);
  if (violations.length > 0) {
    failed = true;
    out("These files are longer than they are allowed to be:\n");
    for (const v of violations) out(`  ${v.path}: ${v.lines}, allowed ${v.allowed}`);
    out(`\nPut the new code in a new file. A file already over the cap may shrink but never grow.\nDo not edit ${BASELINE_FILE} to get a pass.`);
  }
  if (failed) return 1;
  if (opts.update) out(`${BASELINE_FILE} lowered to the current numbers. ${summary(measured)}`);
  else out(`line cap: ok. ${summary(measured)}`);
  if (slack > 0 && !opts.update) out(`${slack} baseline entries can be lowered. Run node scripts/line-cap.mjs --update and commit ${BASELINE_FILE}.`);
  return 0;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exit(run(process.argv.slice(2)));
}
