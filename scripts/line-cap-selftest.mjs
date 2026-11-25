// Tests for scripts/line-cap.mjs. Run with: node --test scripts/line-cap-selftest.mjs
//
// Each test builds a small git repository in a temp directory and runs the real tool on it,
// so what is checked is what CI and a developer see. Named "selftest" and not "test" so the
// unit test runner of the host repo does not pick it up.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import {
  BASELINE_FILE, CONFIG_FILE, LIMITS, compare, countLines, kindOf, newBaseline, parseBaseline,
  parseConfig, run, serialise, tighten, unexplainedRaises,
} from "./line-cap.mjs";

const CONFIG = { include: ["src/", "tool.ts"], extensions: [".ts", ".tsx", ".vue", ".scss", ".go"], exclude: ["src/vendor/"] };
const lines = (n) => "x\n".repeat(n);

function git(root, ...args) {
  return execFileSync("git", ["-C", root, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", ...args], { encoding: "utf8" }).trim();
}

function write(root, rel, content) {
  mkdirSync(dirname(join(root, rel)), { recursive: true });
  writeFileSync(join(root, rel), content);
}

/** A repo with one recorded offender of each kind, files at each cap, and a matching baseline. */
function repo(t, files = {}) {
  const root = mkdtempSync(join(tmpdir(), "line-cap-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  git(root, "init", "-q");
  const all = {
    "src/big.ts": lines(LIMITS.source + 100),
    "src/edge.ts": lines(LIMITS.source),
    "src/big.test.ts": lines(LIMITS.test + 100),
    "src/edge.test.ts": lines(LIMITS.test),
    "src/big.scss": lines(LIMITS.style + 100),
    "src/small.vue": lines(10),
    [CONFIG_FILE]: JSON.stringify(CONFIG),
    ...files,
  };
  for (const [rel, content] of Object.entries(all)) write(root, rel, content);
  git(root, "add", "-A");
  if (!(BASELINE_FILE in files)) {
    assert.equal(tool(root, "--seed").code, 0);
    git(root, "add", "-A");
  }
  git(root, "commit", "-q", "-m", "start");
  return root;
}

function tool(root, ...args) {
  const out = [];
  const code = run(["--root", root, ...args], (s) => out.push(s), (s) => out.push(s));
  return { code, out: out.join("\n") };
}

const baselineOf = (root) => JSON.parse(readFileSync(join(root, BASELINE_FILE), "utf8"));

test("countLines matches wc -l, plus one for an unterminated last line", () => {
  assert.equal(countLines(""), 0);
  assert.equal(countLines("\n"), 1);
  assert.equal(countLines("a"), 1);
  assert.equal(countLines("a\n"), 1);
  assert.equal(countLines("a\nb"), 2);
  assert.equal(countLines(lines(500)), 500);
});

test("kindOf sorts paths into source, test and style", () => {
  const cases = {
    "src/a.ts": "source", "src/Page.vue": "source", "cmd/main.go": "source", "src/contest.ts": "source", "src/latest.spec.md": "source",
    "src/a.test.ts": "test", "src/a.spec.tsx": "test", "internal/x_test.go": "test", "src/__tests__/helper.ts": "test", "src/a.test.mjs": "test",
    "src/a.scss": "style", "src/a.css": "style", "src/a.test.css": "style",
  };
  for (const [path, want] of Object.entries(cases)) assert.equal(kindOf(path), want, path);
});

test("the caps are the ones the owner set", () => {
  assert.deepEqual({ ...LIMITS }, { source: 500, test: 1000, style: 1000 });
  assert.throws(() => { LIMITS.source = 9999; }, TypeError);
});

test("a seeded tree passes, and records only files over their cap", (t) => {
  const root = repo(t);
  assert.deepEqual(baselineOf(root).allowed, {
    "src/big.scss": LIMITS.style + 100, "src/big.test.ts": LIMITS.test + 100, "src/big.ts": LIMITS.source + 100,
  });
  const { code, out } = tool(root);
  assert.equal(code, 0, out);
  assert.match(out, /line cap: ok\. Remaining debt: 1 source files over 500 lines, 1 test files over 1000, 1 stylesheets over 1000\./);
});

test("one line over fails: a recorded file, a new file, and each kind at its boundary", (t) => {
  const root = repo(t);
  write(root, "src/big.ts", lines(LIMITS.source + 101));
  write(root, "src/edge.ts", lines(LIMITS.source + 1));
  write(root, "src/edge.test.ts", lines(LIMITS.test + 1));
  write(root, "src/new.scss", lines(LIMITS.style + 1));
  git(root, "add", "-A");
  const { code, out } = tool(root);
  assert.equal(code, 1);
  for (const want of ["src/big.ts: 601, allowed 600", "src/edge.ts: 501, allowed 500", "src/edge.test.ts: 1001, allowed 1000", "src/new.scss: 1001, allowed 1000", `Do not edit ${BASELINE_FILE}`]) {
    assert.ok(out.includes(want), `missing ${want} in:\n${out}`);
  }
});

test("only tracked, covered, hand-written files are measured", (t) => {
  const root = repo(t);
  write(root, "src/untracked.ts", lines(5000));
  write(root, "src/vendor/lib.ts", lines(5000));
  write(root, "docs/long.ts", lines(5000));
  write(root, "src/readme.md", lines(5000));
  write(root, "src/gen.go", "// Code generated by tool. DO NOT EDIT.\n" + lines(5000));
  write(root, "src/empty.ts", "");
  git(root, "add", "src/vendor", "docs", "src/readme.md", "src/gen.go", "src/empty.ts");
  assert.equal(tool(root).code, 0);
  write(root, "tool.ts", lines(LIMITS.source + 1));
  git(root, "add", "tool.ts");
  assert.equal(tool(root).code, 1, "a file named in include is measured");
});

test("a tracked file deleted from the working tree is not an error", (t) => {
  const root = repo(t);
  rmSync(join(root, "src/small.vue"));
  rmSync(join(root, "src/big.ts"));
  const { code, out } = tool(root);
  assert.equal(code, 0, out);
  assert.match(out, /1 baseline entries can be lowered/);
});

test("--update lowers and removes, and never raises or adds", (t) => {
  const root = repo(t);
  write(root, "src/big.ts", lines(LIMITS.source + 50));
  write(root, "src/big.scss", lines(LIMITS.style));
  assert.equal(tool(root, "--update").code, 0);
  assert.deepEqual(baselineOf(root).allowed, { "src/big.test.ts": LIMITS.test + 100, "src/big.ts": LIMITS.source + 50 });

  write(root, "src/big.ts", lines(LIMITS.source + 400));
  write(root, "src/fresh.ts", lines(LIMITS.source + 1));
  git(root, "add", "-A");
  const { code, out } = tool(root, "--update");
  assert.equal(code, 1, out);
  assert.deepEqual(baselineOf(root).allowed, { "src/big.test.ts": LIMITS.test + 100, "src/big.ts": LIMITS.source + 50 });
});

test("tighten and compare on plain data", () => {
  const b = newBaseline({ "same.ts": 900, "shrunk.ts": 900, "paid.ts": 900, "worse.ts": 600 }, [{ key: "same.ts" }]);
  const m = { "same.ts": 900, "shrunk.ts": 700, "worse.ts": 650, "new.ts": 501 };
  assert.deepEqual(tighten(m, b).allowed, { "same.ts": 900, "shrunk.ts": 700, "worse.ts": 600 });
  assert.deepEqual(tighten(m, b).exceptions, [{ key: "same.ts" }]);
  const { violations, slack } = compare(m, b);
  assert.deepEqual(violations, [{ path: "new.ts", lines: 501, allowed: 500 }, { path: "worse.ts", lines: 650, allowed: 600 }]);
  assert.equal(slack, 2);
  assert.deepEqual(compare({}, newBaseline()), { violations: [], slack: 0 });
});

test("a baseline it does not fully understand is refused", () => {
  const ok = newBaseline({ "a.ts": 600 });
  assert.deepEqual(parseBaseline(serialise(ok)), ok);
  const bad = {
    "not json": "{",
    "empty": "",
    "an array": "[]",
    "unknown key": JSON.stringify({ ...ok, caps: {} }),
    "wrong version": JSON.stringify({ ...ok, version: 2 }),
    "no limits": JSON.stringify({ version: 1, allowed: {}, exceptions: [] }),
    "a raised cap": JSON.stringify({ ...ok, limits: { ...LIMITS, source: 99999 } }),
    "a lowered cap": JSON.stringify({ ...ok, limits: { ...LIMITS, test: 999 } }),
    "an entry at the cap": JSON.stringify(newBaseline({ "a.ts": 500 })),
    "a test entry at its cap": JSON.stringify(newBaseline({ "a.test.ts": 1000 })),
    "a number as text": JSON.stringify(newBaseline({ "a.ts": "600" })),
    "a fraction": JSON.stringify(newBaseline({ "a.ts": 600.5 })),
    "exceptions not a list": JSON.stringify({ ...ok, exceptions: {} }),
  };
  for (const [name, text] of Object.entries(bad)) assert.throws(() => parseBaseline(text), undefined, name);
  assert.equal(parseBaseline(bad["a raised cap"], { strict: false }).limits.source, 99999, "an older commit's caps are read as they were");
});

test("a config that would check nothing is refused", () => {
  assert.deepEqual(parseConfig(JSON.stringify({ include: ["src/"], extensions: [".ts"] })).exclude, []);
  for (const text of ["{", "{}", '{"include":[],"extensions":[".ts"]}', '{"include":["src/"],"extensions":[]}', '{"include":"src/","extensions":[".ts"]}', '{"include":[""],"extensions":[".ts"]}', '{"include":["src/"],"extensions":[".ts"],"limits":{}}']) {
    assert.throws(() => parseConfig(text), undefined, text);
  }
});

test("unexplainedRaises needs a complete exception naming the exact value", () => {
  const old = newBaseline({ "a.ts": 800, "b.ts": 700 });
  const good = { key: "a.ts", to: 900, reason: "the refund flow had to land in this file", approved_by: "owner", date: "2026-11-23" };
  const cases = [
    ["unchanged", { "a.ts": 800, "b.ts": 700 }, [], []],
    ["lowered and removed", { "a.ts": 700 }, [], []],
    ["raised, no exception", { "a.ts": 801, "b.ts": 700 }, [], ["a.ts"]],
    ["raised, valid exception", { "a.ts": 900, "b.ts": 700 }, [good], []],
    ["exception for another value", { "a.ts": 901, "b.ts": 700 }, [good], ["a.ts"]],
    ["exception for another file", { "a.ts": 800, "b.ts": 900 }, [good], ["b.ts"]],
    ["new entry over the cap", { "a.ts": 800, "b.ts": 700, "c.ts": 501, "d.test.ts": 1001 }, [], ["c.ts", "d.test.ts"]],
    ["short reason", { "a.ts": 900 }, [{ ...good, reason: "needed" }], ["a.ts"]],
    ["no approver", { "a.ts": 900 }, [{ ...good, approved_by: " " }], ["a.ts"]],
    ["bad date", { "a.ts": 900 }, [{ ...good, date: "23 Nov" }], ["a.ts"]],
    ["a null exception", { "a.ts": 900 }, [null], ["a.ts"]],
  ];
  for (const [name, allowed, exceptions, want] of cases) {
    assert.deepEqual(unexplainedRaises(newBaseline(allowed, exceptions), old).map((r) => r.path), want, name);
  }
  // Against a commit whose caps were looser, a file that met those caps is a record, not a raise.
  const looser = { ...newBaseline(), limits: { source: 1500, test: 1000, style: 1000 } };
  assert.deepEqual(unexplainedRaises(newBaseline({ "mid.ts": 900, "huge.ts": 1501 }), looser).map((r) => r.path), ["huge.ts"]);
});

test("--base refuses a baseline edited upwards, and passes with an exception", (t) => {
  const root = repo(t);
  write(root, "src/big.ts", lines(LIMITS.source + 200));
  const b = baselineOf(root);
  b.allowed["src/big.ts"] = LIMITS.source + 200;
  write(root, BASELINE_FILE, serialise(b));
  assert.equal(tool(root).code, 0, "without --base the doctored baseline matches the tree");
  const { code, out } = tool(root, "--base", "HEAD");
  assert.equal(code, 1);
  assert.ok(out.includes("src/big.ts: 600 -> 700"), out);

  b.exceptions = [{ key: "src/big.ts", to: LIMITS.source + 200, reason: "the checkout flow had to land here", approved_by: "owner", date: "2026-11-23" }];
  write(root, BASELINE_FILE, serialise(b));
  assert.equal(tool(root, "--base", "HEAD").code, 0);
});

test("--base against a commit with no baseline passes, and an unknown commit is an error", (t) => {
  const root = repo(t);
  git(root, "rm", "-q", "--cached", BASELINE_FILE);
  git(root, "commit", "-q", "-m", "before the ratchet");
  assert.equal(tool(root, "--base", "HEAD").code, 0);
  const { code, out } = tool(root, "--base", "0000000000000000000000000000000000000000");
  assert.equal(code, 2);
  assert.match(out, /cannot find commit/);
});

test("it fails loudly when it cannot run", (t) => {
  const root = repo(t);
  assert.equal(tool(root, "--seed").code, 2, "seed over an existing baseline");
  assert.equal(tool(root, "--update", "--seed").code, 2);
  assert.equal(tool(root, "--nonsense").code, 2);
  assert.equal(tool(root, "--base").code, 2, "a flag with no value");

  write(root, BASELINE_FILE, "{");
  assert.equal(tool(root).code, 2, "corrupt baseline");
  rmSync(join(root, BASELINE_FILE));
  assert.match(tool(root).out, /is missing\. It must be committed/);
  rmSync(join(root, CONFIG_FILE));
  assert.equal(tool(root).code, 2, "no config");

  const notARepo = mkdtempSync(join(tmpdir(), "line-cap-"));
  t.after(() => rmSync(notARepo, { recursive: true, force: true }));
  write(notARepo, CONFIG_FILE, JSON.stringify(CONFIG));
  const { code, out } = tool(notARepo);
  assert.equal(code, 2);
  assert.match(out, /cannot list tracked files/);
});

test("a baseline that cannot be written is an error and is left as it was", { skip: process.getuid?.() === 0 }, (t) => {
  const root = repo(t);
  write(root, "src/big.ts", lines(LIMITS.source + 50));
  const before = readFileSync(join(root, BASELINE_FILE), "utf8");
  chmodSync(join(root, BASELINE_FILE), 0o444);
  const { code, out } = tool(root, "--update");
  assert.equal(code, 2);
  assert.match(out, /cannot write/);
  assert.equal(readFileSync(join(root, BASELINE_FILE), "utf8"), before);
});

test("the command line entry point exits with the tool's code", (t) => {
  const root = repo(t);
  const script = new URL("./line-cap.mjs", import.meta.url).pathname;
  assert.match(execFileSync("node", [script, "--root", root], { encoding: "utf8" }), /line cap: ok/);
  write(root, "src/edge.ts", lines(LIMITS.source + 1));
  assert.throws(() => execFileSync("node", [script, "--root", root], { stdio: "pipe" }), (e) => e.status === 1);
});

test("many checks of one tree at once agree", async (t) => {
  const root = repo(t);
  const results = await Promise.all(Array.from({ length: 12 }, async () => tool(root, "--base", "HEAD").code));
  assert.deepEqual(results, Array(12).fill(0));
});
