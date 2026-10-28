import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterAll, afterEach, beforeAll, describe, expect, test } from "vitest";
import { buildDaemon, Daemon, TestRoot, waitFor } from "./daemon-env.ts";
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

/** A tool that runs until the test creates `release` in the workspace; it creates `waiting` when it starts. */
const WAIT_TOOL = `import { existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { Type } from "typebox";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.registerTool({
    name: "e2e_wait",
    label: "e2e_wait",
    description: "等待测试放行，然后返回“已放行”。",
    parameters: Type.Object({}),
    async execute(_toolCallId, _params, signal, _onUpdate, ctx) {
      writeFileSync(join(ctx.cwd, "waiting"), "");
      while (!existsSync(join(ctx.cwd, "release"))) {
        if (signal?.aborted) throw new Error("aborted");
        await new Promise((resolve) => setTimeout(resolve, 200));
      }
      return { content: [{ type: "text", text: "已放行" }], details: {} };
    },
  });
}
`;

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

  const baseUrl = process.env.BEARBOT_TEST_OPENAI_BASE_URL;
  const apiKey = process.env.BEARBOT_TEST_OPENAI_API_KEY;
  const model = process.env.BEARBOT_TEST_OPENAI_MODEL;
  const haveModel = Boolean(baseUrl && apiKey && model);

  test.skipIf(!haveModel)("a question during a running tool call is answered before the turn ends (needs BEARBOT_TEST_OPENAI_BASE_URL, _API_KEY, _MODEL)", async () => {
    const env = newRoot();
    env.writeAgentProfileFile("models.json", JSON.stringify({
      providers: { testprovider: { baseUrl, api: "openai-completions", apiKey, models: [{ id: model }] } },
    }));
    env.writeAgentProfileFile("settings.json", JSON.stringify({
      quietStartup: true,
      defaultProjectTrust: "never",
      defaultProvider: "testprovider",
      defaultModel: model,
    }));
    env.writeWorkspaceFile(".pi/extensions/wait.ts", WAIT_TOOL);
    const d = await start(env);
    await d.waitRunning(90_000);
    await d.waitReady(1, 60_000);

    d.send("请调用 e2e_wait 工具，等它返回后只回复“完成”两个字。");
    await waitFor("e2e_wait 开始执行", 120_000, () => existsSync(join(env.workspace, "waiting")));

    // Lines from here on: the console may echo the input lines above.
    let mark = d.lines.length;
    const question = "你现在在等哪个工具返回？";
    d.send(`!bb ${question}`);
    await d.waitLine("e2e_wait", 120_000, mark);
    expect(d.linesWith("btw 提问失败", mark)).toEqual([]);
    // The main turn is still waiting: no final reply yet, and the question is not in the main session.
    expect(d.linesWith("完成", mark)).toEqual([]);
    expect(Object.values(sessionFiles(env)).some((content) => content.includes(question))).toBe(false);

    mark = d.lines.length;
    writeFileSync(join(env.workspace, "release"), "");
    await d.waitLine("完成", 120_000, mark);
    expect(Object.values(sessionFiles(env)).some((content) => content.includes(question))).toBe(false);
  }, 400_000);
});
