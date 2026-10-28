import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  createAgentSession,
  DefaultResourceLoader,
  type ExtensionAPI,
  type ExtensionError,
  type ExtensionFactory,
  SessionManager,
} from "@earendil-works/pi-coding-agent";
import { afterAll, beforeAll, describe, expect, test } from "vitest";
import { type BearbotApi, type InboundMiddleware, type OutboundMiddleware, useBearbot } from "../../../sdk/index.ts";
import bearbotIm from "../index.ts";
import { getBridge } from "../src/bridge.ts";
import { EXTENSION_DIR, agentEnv, removeTempDirs, tempDir } from "./helpers.ts";

afterAll(removeTempDirs);

// Real pi sessions in this process: bearbot-im in trial mode plus an extension that uses the SDK.

let root: string;
let workspace: string;

beforeAll(() => {
  root = tempDir("bbim-sdk-");
  workspace = join(root, "workspace");
  mkdirSync(workspace, { recursive: true });
  mkdirSync(join(root, "agent"), { recursive: true });
  mkdirSync(join(root, "home"), { recursive: true });
  writeFileSync(join(workspace, "AGENTS.md"), "# 记忆\n");
  Object.assign(process.env, agentEnv(root, workspace, { BEARBOT_TRIAL: "1" }));
});

async function startSession(factories: { name: string; factory: ExtensionFactory }[]) {
  const loader = new DefaultResourceLoader({
    cwd: workspace,
    agentDir: join(root, "agent"),
    extensionFactories: factories,
    noSkills: true,
    noPromptTemplates: true,
    noThemes: true,
    disabledBuiltinExtensions: ["mcp", "codemode", "tool-search", "llama.cpp"],
  });
  await loader.reload();
  const { session } = await createAgentSession({
    cwd: workspace,
    agentDir: join(root, "agent"),
    resourceLoader: loader,
    sessionManager: SessionManager.inMemory(),
  });
  const errors: ExtensionError[] = [];
  await session.bindExtensions({ onError: (error) => errors.push(error) });
  return { session, errors };
}

describe("useBearbot", () => {
  test("buffers registrations until session_start and applies them in order", async () => {
    const outcome: Record<string, unknown> = {};
    let handle: BearbotApi | undefined;
    const late: InboundMiddleware = (msg, next) => next(msg);
    const early: InboundMiddleware = (msg, next) => next(msg);
    const outbound: OutboundMiddleware = (reply, next) => next(reply);

    const probe: ExtensionFactory = (pi: ExtensionAPI) => {
      const bearbot = useBearbot(pi, { name: "probe" });
      handle = bearbot;
      try {
        void bearbot.trial;
        outcome.trialBeforeConnect = "no error";
      } catch (error) {
        outcome.trialBeforeConnect = (error as Error).message;
      }
      bearbot.use(late, { order: 20 });
      bearbot.use(early, { order: 10 });
      const cancelled = bearbot.onMessage(() => {});
      cancelled();
      bearbot.useOutbound(outbound, { order: 1 });
      bearbot.onTurnEnd(() => {});
      pi.on("session_start", async () => {
        outcome.sessionStarts = Number(outcome.sessionStarts ?? 0) + 1;
        outcome.trial = bearbot.trial;
        outcome.workspace = bearbot.workspace.dir;
        outcome.post = await bearbot.post("telegram:1", { markdown: "x" }).then(() => "sent", (error: Error) => error.message);
        outcome.prompt = await bearbot.prompt({ text: "x", role: "member" }).then(() => "queued", (error: Error) => error.message);
        try {
          bearbot.bindings();
        } catch (error) {
          outcome.bindings = (error as Error).message;
        }
      });
    };

    const { session, errors } = await startSession([
      { name: "bearbot-im", factory: bearbotIm },
      { name: "probe", factory: probe },
    ]);
    expect(errors).toEqual([]);
    expect(outcome.trialBeforeConnect).toContain("尚未连接");
    expect(outcome.trial).toBe(true);
    expect(outcome.workspace).toBe(workspace);
    expect(outcome.post).toContain("试运行中不能调用");
    expect(outcome.prompt).toContain("试运行中不能调用");
    expect(outcome.bindings).toContain("试运行中不能调用");

    const bridge = getBridge(EXTENSION_DIR);
    expect(bridge.middleware.list().map((entry) => [entry.owner.name, entry.order])).toEqual([["probe", 10], ["probe", 20]]);
    expect(bridge.middleware.list().map((entry) => entry.fn)).toEqual([early, late]);
    expect(bridge.subscribers.list()).toHaveLength(0);
    expect(bridge.outboundMiddleware.list().map((entry) => entry.fn)).toEqual([outbound]);
    expect(bridge.turnEndListeners.list()).toHaveLength(1);

    // Registrations after the connection go straight to bearbot-im; a repeated session_start changes nothing.
    const unsubscribe = handle?.use((msg, next) => next(msg), { order: 15 });
    expect(bridge.middleware.list().map((entry) => entry.order)).toEqual([10, 15, 20]);
    unsubscribe?.();
    await session.bindExtensions({ onError: (error) => errors.push(error) });
    expect(outcome.sessionStarts).toBe(2);
    expect(errors).toEqual([]);
    expect(bridge.middleware.list().map((entry) => entry.order)).toEqual([10, 20]);
    session.dispose();
  });

  test("throws 'bearbot-im 未加载' in session_start when bearbot-im is missing", async () => {
    const lonely: ExtensionFactory = (pi: ExtensionAPI) => {
      useBearbot(pi, { name: "lonely" }).onMessage(() => {});
    };
    const { session, errors } = await startSession([{ name: "lonely", factory: lonely }]);
    expect(errors.map((error) => [error.event, error.error])).toEqual([["session_start", "bearbot-im 未加载"]]);
    session.dispose();
  });

  test("rejects an empty name in the factory", async () => {
    const nameless: ExtensionFactory = (pi: ExtensionAPI) => {
      useBearbot(pi, { name: " " });
    };
    const loader = new DefaultResourceLoader({
      cwd: workspace,
      agentDir: join(root, "agent"),
      extensionFactories: [{ name: "nameless", factory: nameless }],
      noSkills: true,
      noPromptTemplates: true,
      noThemes: true,
    });
    await loader.reload();
    const failures = loader.getExtensions().errors.map((entry) => entry.error);
    expect(failures.some((error) => error.includes("useBearbot 需要非空的 name"))).toBe(true);
  });
});
