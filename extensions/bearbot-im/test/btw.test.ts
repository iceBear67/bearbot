import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { afterAll, afterEach, beforeAll, describe, expect, test } from "vitest";
import { buildDaemon, Daemon, TestRoot } from "./daemon-env.ts";
import { removeTempDirs, tempDir } from "./helpers.ts";

// `!bb <问题>` through the real bearbotd, gateway, bearbot-im and pi (DESIGN 6.9, 7.11).

afterAll(removeTempDirs);

let bin: string;

beforeAll(async () => {
  bin = await buildDaemon(tempDir("bbim-btw-bin-"));
}, 600_000);

const roots: TestRoot[] = [];
const daemons: Daemon[] = [];

afterEach(async () => {
  for (const daemon of daemons.splice(0)) await daemon.stop();
  for (const root of roots.splice(0)) root.remove();
});

async function start(env: TestRoot): Promise<Daemon> {
  const daemon = await Daemon.start(bin, env);
  daemons.push(daemon);
  return daemon;
}

function newRoot(): TestRoot {
  const env = new TestRoot();
  roots.push(env);
  return env;
}

/** Contents of the agent's session files, by path. */
function sessionFiles(env: TestRoot): Record<string, string> {
  const files: Record<string, string> = {};
  const walk = (dir: string): void => {
    if (!existsSync(dir)) return;
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) walk(path);
      else files[path] = readFileSync(path, "utf8");
    }
  };
  walk(env.path("/bearbot/profile/agent/sessions"));
  return files;
}

describe("!bb with the real daemon, gateway and pi", () => {
  test("the question goes to a separate pi session; without credentials its error is the reply and the main session is unchanged", async () => {
    const env = newRoot();
    const d = await start(env);
    await d.waitRunning(90_000);
    await d.waitReady(1, 60_000);

    const before = sessionFiles(env);
    const mark = d.lines.length;
    d.send("!bb 你在做什么");
    await d.waitLine("btw 提问失败：No API key found for", 60_000, mark);
    expect(sessionFiles(env)).toEqual(before);
    expect(env.diagnostics()).toEqual([]);
  }, 300_000);
});
