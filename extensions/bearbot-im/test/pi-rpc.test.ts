import { existsSync, mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterAll, afterEach, beforeAll, describe, expect, test } from "vitest";
import { agentArgs, agentEnv, EXTENSION_DIR, type PiLayout, piLayout as layout, removeTempDirs, RpcProcess, SDK_FILE } from "./helpers.ts";

afterAll(removeTempDirs);

// Tests that start the real pi with bearbot-im: `pi --mode rpc --no-session -e <bearbot-im>` (contracts 2.2).

const BEARBOT_TOOLS = ["im_send", "im_history", "im_targets", "bearbot_status", "bearbot_logs", "bearbot_check", "bearbot_apply", "bearbot_snapshot"];

const PROBE = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from ${JSON.stringify(SDK_FILE)};

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "probe" });
  const results: Record<string, unknown> = {};
  results.useReturned = typeof bearbot.use((msg, next) => next(msg), { order: 10 });
  results.onMessageReturned = typeof bearbot.onMessage(() => {});
  results.onTurnEndReturned = typeof bearbot.onTurnEnd(() => {});
  pi.on("session_start", async () => {
    results.trial = bearbot.trial;
    results.workspace = bearbot.workspace.dir;
    results.apiVersion = bearbot.apiVersion;
    results.post = await bearbot.post("telegram:1", { markdown: "hi" }).then(() => "sent", (error: Error) => error.message);
    results.history = await bearbot.history("telegram:1").then(() => "ok", (error: Error) => error.message);
    results.prompt = await bearbot.prompt({ text: "hi", role: "admin" }).then(() => "ok", (error: Error) => error.message);
    try {
      bearbot.bindings();
      results.bindings = "ok";
    } catch (error) {
      results.bindings = (error as Error).message;
    }
    try {
      bearbot.isAdmin("telegram:1");
      results.isAdmin = "ok";
    } catch (error) {
      results.isAdmin = (error as Error).message;
    }
    results.lateUse = typeof bearbot.useOutbound((reply, next) => next(reply), { order: 1 });
    bearbot.diagnostic({ level: "warning", message: "probe diagnostic" });
  });
  pi.registerCommand("bbprobe", {
    description: "report probe results",
    handler: async (_args, ctx) => {
      ctx.ui.notify(JSON.stringify({
        ...results,
        tools: pi.getAllTools().map((tool) => tool.name),
        active: pi.getActiveTools(),
        trusted: ctx.isProjectTrusted(),
      }), "info");
    },
  });
}
`;

const CLASSIFY = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { readFileSync } from "node:fs";
import { judgeWithModel, buildClassifierRequest } from ${JSON.stringify(join(EXTENSION_DIR, "src", "classifier.ts"))};

export default function (pi: ExtensionAPI) {
  pi.registerCommand("bbclassify", {
    description: "run one classifier judgment",
    handler: async (args, ctx) => {
      const { provider, model, timeoutMs, command, message } = JSON.parse(args);
      const found = ctx.modelRegistry.find(provider, model);
      if (!found) {
        ctx.ui.notify(JSON.stringify({ kind: "no-model" }), "info");
        return;
      }
      const system = readFileSync(${JSON.stringify(join(EXTENSION_DIR, "prompt", "classifier.md"))}, "utf8")
        .split("{{RUN_DIR}}").join("/run/bearbot").split("{{PROFILE_DIR}}").join("/bearbot/profile");
      const request = buildClassifierRequest({
        workspace: ctx.cwd,
        messages: [{ from: "Bob (telegram:2)", role: "member", text: message }],
        toolName: "bash",
        tool: pi.getAllTools().find((tool) => tool.name === "bash"),
        args: { command },
        nested: false,
      });
      const verdict = await judgeWithModel(ctx, found, system, request, timeoutMs);
      ctx.ui.notify(JSON.stringify(verdict), "info");
    },
  });
}
`;

const running: RpcProcess[] = [];

afterEach(async () => {
  for (const process of running.splice(0)) await process.stop();
});

function startPi(cwd: string, env: Record<string, string>): RpcProcess {
  const pi = new RpcProcess(["--no-session", ...agentArgs()], { cwd, env });
  running.push(pi);
  return pi;
}

function commandNames(response: Record<string, unknown>): string[] {
  const data = response.data as { commands: { name: string }[] };
  return data.commands.map((command) => command.name);
}

async function probeResults(pi: RpcProcess): Promise<Record<string, unknown>> {
  const before = pi.notifications().length;
  const response = await pi.request("probe", { type: "prompt", message: "/bbprobe" });
  expect(response.success).toBe(true);
  const message = await pi.waitFor(() => pi.notifications()[before], "probe 的结果");
  return JSON.parse(message) as Record<string, unknown>;
}

describe("bearbot-im in the real pi (trial mode)", () => {
  let trusted: PiLayout;

  beforeAll(() => {
    trusted = layout("bbim-rpc-", { "probe.ts": PROBE });
  });

  test("starts, answers get_state and registers tools, skills and the trusted project extension", async () => {
    const pi = startPi(trusted.workspace, agentEnv(trusted.root, trusted.workspace, { BEARBOT_TRIAL: "1" }));
    const state = await pi.request("boot", { type: "get_state" });
    expect(state.success).toBe(true);
    const commands = commandNames(await pi.request("cmds", { type: "get_commands" }));
    expect(commands).toContain("bbprobe");
    expect(commands).toContain("skill:bearbot-extension-dev");
    expect(commands).toContain("skill:bearbot-middleware");
    // bearbot-recovery is only for the recovery process (DESIGN 9.1).
    expect(commands).not.toContain("skill:bearbot-recovery");
    const results = await probeResults(pi);
    for (const tool of BEARBOT_TOOLS) expect(results.tools).toContain(tool);
    // DESIGN 16 内置工具: read, write, edit, bash, grep, find and ls are enabled in the agent process.
    for (const tool of ["read", "write", "edit", "bash", "grep", "find", "ls", ...BEARBOT_TOOLS]) expect(results.active).toContain(tool);
    expect(results.trusted).toBe(true);
  });

  test("the SDK works in trial mode: flag set, registrations work, gateway methods throw", async () => {
    const pi = startPi(trusted.workspace, agentEnv(trusted.root, trusted.workspace, { BEARBOT_TRIAL: "1" }));
    await pi.request("boot", { type: "get_state" });
    const results = await probeResults(pi);
    expect(results).toMatchObject({
      trial: true,
      workspace: trusted.workspace,
      apiVersion: 1,
      useReturned: "function",
      onMessageReturned: "function",
      onTurnEndReturned: "function",
      lateUse: "function",
    });
    for (const method of ["post", "history", "prompt", "bindings", "isAdmin"]) {
      expect(String(results[method])).toContain("试运行中不能调用");
    }
    expect(pi.stderr).toContain("[bearbot 诊断 warning] extension:probe：probe diagnostic");
    expect(pi.records.some((record) => record.type === "extension_error")).toBe(false);
  });

  test("trial mode connects to neither the gateway nor agent.sock", async () => {
    const env = agentEnv(trusted.root, trusted.workspace, { BEARBOT_TRIAL: "1" });
    // Both sockets are missing: any connection attempt would fail and be reported on stderr, as the normal mode
    // test below shows for the gateway.
    expect(existsSync(env.BEARBOT_GATEWAY_SOCKET)).toBe(false);
    expect(existsSync(env.BEARBOT_AGENT_SOCKET)).toBe(false);
    const pi = startPi(trusted.workspace, env);
    expect((await pi.request("boot", { type: "get_state" })).success).toBe(true);
    await new Promise((resolve) => setTimeout(resolve, 2000));
    for (const text of ["gateway 连接", "gateway 拒绝了 hello", "agent.sock", "/v1/bridge"]) expect(pi.stderr).not.toContain(text);
    expect(pi.records.some((record) => record.type === "extension_error")).toBe(false);
  });

  test("input that bearbot-im did not start is handled and reported", async () => {
    const pi = startPi(trusted.workspace, agentEnv(trusted.root, trusted.workspace, { BEARBOT_TRIAL: "1" }));
    await pi.request("boot", { type: "get_state" });
    const response = await pi.request("p", { type: "prompt", message: "你好" });
    expect(response).toMatchObject({ success: true, data: { disposition: "handled" } });
    expect(pi.stderr).toContain("拒绝了不是由 bearbot-im 发起的输入（来源 rpc）");
    expect(pi.records.some((record) => record.type === "agent_start")).toBe(false);
  });

  test("project trust is granted only to the workspace directory", async () => {
    const other = join(trusted.root, "elsewhere");
    mkdirSync(join(other, ".pi", "extensions"), { recursive: true });
    writeFileSync(join(other, ".pi", "extensions", "probe.ts"), PROBE);
    const pi = startPi(other, agentEnv(trusted.root, trusted.workspace, { BEARBOT_TRIAL: "1" }));
    await pi.request("boot", { type: "get_state" });
    const commands = commandNames(await pi.request("cmds", { type: "get_commands" }));
    expect(commands).not.toContain("bbprobe");
  });

  test("the workspace given through a symlink is still trusted (realpath comparison)", async () => {
    const link = join(trusted.root, "workspace-link");
    symlinkSync(trusted.workspace, link);
    const pi = startPi(trusted.workspace, agentEnv(trusted.root, link, { BEARBOT_TRIAL: "1" }));
    await pi.request("boot", { type: "get_state" });
    expect(commandNames(await pi.request("cmds", { type: "get_commands" }))).toContain("bbprobe");
  });

  test("a missing environment variable makes the load fail", async () => {
    const env = agentEnv(trusted.root, trusted.workspace, { BEARBOT_TRIAL: "1" });
    delete env.BEARBOT_ERROR_NOTICE;
    const pi = startPi(trusted.workspace, env);
    pi.send({ id: "boot", type: "get_state" });
    const exit = await pi.exited;
    expect(exit.code).toBe(1);
    expect(pi.stderr).toContain("bearbot-im 缺少环境变量 BEARBOT_ERROR_NOTICE");
  });

  test("normal mode keeps starting without a gateway and writes diagnostics into the workspace", async () => {
    const own = layout("bbim-rpc-normal-", {});
    const pi = startPi(own.workspace, agentEnv(own.root, own.workspace));
    expect((await pi.request("boot", { type: "get_state" })).success).toBe(true);
    await pi.request("p", { type: "prompt", message: "你好" });
    await pi.waitFor(() => (pi.stderr.includes("gateway 连接失败") ? true : undefined), "gateway 重连的日志");
    const lines = readFileSync(join(own.workspace, ".bearbot", "diagnostics.jsonl"), "utf8").trim().split("\n");
    const record = JSON.parse(lines[0]) as { source: string; level: string; delivered: boolean };
    expect(record).toMatchObject({ source: "input", level: "warning", delivered: false });
    expect((await pi.stop()).code).toBe(0);
  });
});

describe("classifier model call in the real pi", () => {
  test("a model without credentials gives a classifier error", async () => {
    const own = layout("bbim-rpc-cls-", { "classify.ts": CLASSIFY });
    const pi = startPi(own.workspace, agentEnv(own.root, own.workspace, { BEARBOT_TRIAL: "1" }));
    await pi.request("boot", { type: "get_state" });
    const args = JSON.stringify({ provider: "anthropic", model: "claude-haiku-4-5-20251001", timeoutMs: 10000, command: "ls docs", message: "看看 docs 里有什么" });
    await pi.request("c", { type: "prompt", message: `/bbclassify ${args}` });
    const verdict = JSON.parse(await pi.waitFor(() => pi.notifications()[0], "classifier 结果")) as { kind: string; error?: string };
    expect(verdict.kind).toBe("failed");
    expect(verdict.error).toContain("anthropic");
  });
});
