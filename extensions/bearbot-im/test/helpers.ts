import { type ChildProcessWithoutNullStreams, spawn } from "node:child_process";
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import type { ToolInfo } from "@earendil-works/pi-coding-agent";
import type { WireMessage } from "../src/gateway-client.ts";

export const TEST_DIR = dirname(fileURLToPath(import.meta.url));
export const EXTENSION_DIR = resolve(TEST_DIR, "..");
export const REPO = resolve(EXTENSION_DIR, "..", "..");
export const SDK_FILE = join(REPO, "sdk", "index.ts");
export const PI_BIN = join(REPO, "node_modules", ".bin", "pi");

const createdDirs: string[] = [];

export function tempDir(prefix: string): string {
  const dir = realpathSync(mkdtempSync(join(tmpdir(), prefix)));
  createdDirs.push(dir);
  return dir;
}

/** Removes every directory made by tempDir in this test file; register with afterAll. */
export function removeTempDirs(): void {
  for (const dir of createdDirs.splice(0)) rmSync(dir, { recursive: true, force: true });
}

let seqCounter = 0;

export function wire(overrides: Partial<WireMessage> & { seq?: number[] } = {}): WireMessage {
  seqCounter += 1;
  return {
    seq: [seqCounter],
    kind: "message",
    id: `m${seqCounter}`,
    platform: "telegram",
    channel: { id: "telegram:-100123", name: "测试群", isDM: false },
    thread: "telegram:-100123",
    sender: { id: "telegram:123", name: "Alice", role: "member", isBot: false },
    time: "2026-10-26T14:02:11+08:00",
    text: `text ${seqCounter}`,
    mentionsBot: true,
    edited: false,
    stale: false,
    attachments: [],
    trigger: true,
    ...overrides,
  };
}

/** Environment of the agent process (contracts 2.1) for a test workspace. */
export function agentEnv(root: string, workspace: string, extra: Record<string, string> = {}): Record<string, string> {
  return {
    PATH: process.env.PATH ?? "/usr/bin:/bin",
    HOME: join(root, "home"),
    PI_CODING_AGENT_DIR: join(root, "agent"),
    PI_SKIP_VERSION_CHECK: "1",
    PI_OFFLINE: "1",
    BEARBOT_WORKSPACE_DIR: workspace,
    BEARBOT_GATEWAY_SOCKET: join(root, "gateway.sock"),
    BEARBOT_AGENT_SOCKET: join(root, "agent.sock"),
    BEARBOT_CLASSIFIER_MODEL: "anthropic/claude-haiku-4-5-20251001",
    BEARBOT_CLASSIFIER_TIMEOUT: "15000",
    BEARBOT_CLASSIFIER_RULES_MAX_BYTES: "32768",
    BEARBOT_OBSERVE_LIMIT: "30",
    BEARBOT_HOLD_WARN_AFTER: "600000",
    BEARBOT_ERROR_NOTICE: "处理时出现错误，已通知管理员。",
    ...extra,
  };
}

export type RpcRecord = Record<string, unknown> & { type: string };

/** A real `pi --mode rpc` process; records stdout records and stderr. */
export class RpcProcess {
  readonly child: ChildProcessWithoutNullStreams;
  readonly records: RpcRecord[] = [];
  stderr = "";
  private buffer = "";
  readonly exited: Promise<{ code: number | null; signal: NodeJS.Signals | null }>;
  private listeners = new Set<() => void>();

  constructor(args: string[], options: { cwd: string; env: Record<string, string> }) {
    this.child = spawn(PI_BIN, ["--mode", "rpc", ...args], { cwd: options.cwd, env: options.env, stdio: ["pipe", "pipe", "pipe"] });
    this.child.stdout.setEncoding("utf8");
    this.child.stdout.on("data", (chunk: string) => {
      this.buffer += chunk;
      let index = this.buffer.indexOf("\n");
      while (index !== -1) {
        const line = this.buffer.slice(0, index);
        this.buffer = this.buffer.slice(index + 1);
        if (line.trim() !== "") this.records.push(JSON.parse(line) as RpcRecord);
        index = this.buffer.indexOf("\n");
      }
      this.changed();
    });
    this.child.stderr.setEncoding("utf8");
    this.child.stderr.on("data", (chunk: string) => {
      this.stderr += chunk;
      this.changed();
    });
    this.exited = new Promise((resolveExit) => {
      this.child.on("exit", (code, signal) => {
        this.changed();
        resolveExit({ code, signal });
      });
    });
  }

  private changed(): void {
    for (const listener of [...this.listeners]) listener();
  }

  send(command: Record<string, unknown>): void {
    this.child.stdin.write(`${JSON.stringify(command)}\n`);
  }

  waitFor<T>(find: () => T | undefined, what: string, timeoutMs = 30_000): Promise<T> {
    return new Promise<T>((resolveWait, reject) => {
      const check = () => {
        const found = find();
        if (found !== undefined) {
          cleanup();
          resolveWait(found);
        } else if (this.child.exitCode !== null || this.child.signalCode !== null) {
          cleanup();
          reject(new Error(`pi 已退出（${this.child.exitCode ?? this.child.signalCode}），没有等到 ${what}；stderr：${this.stderr}`));
        }
      };
      const timer = setTimeout(() => {
        cleanup();
        reject(new Error(`等待 ${what} 超时；stderr：${this.stderr}`));
      }, timeoutMs);
      const cleanup = () => {
        clearTimeout(timer);
        this.listeners.delete(check);
      };
      this.listeners.add(check);
      check();
    });
  }

  response(id: string): Promise<RpcRecord> {
    return this.waitFor(() => this.records.find((record) => record.type === "response" && record.id === id), `响应 ${id}`);
  }

  async request(id: string, command: Record<string, unknown>): Promise<RpcRecord> {
    this.send({ id, ...command });
    return this.response(id);
  }

  notifications(): string[] {
    return this.records
      .filter((record) => record.type === "extension_ui_request" && record.method === "notify")
      .map((record) => String(record.message));
  }

  async stop(): Promise<{ code: number | null; signal: NodeJS.Signals | null }> {
    if (this.child.exitCode === null && this.child.signalCode === null) this.child.stdin.end();
    return this.exited;
  }
}

export interface PiLayout {
  root: string;
  workspace: string;
}

/** A test root with a workspace (containing the given project extensions), a pi profile and a home directory. */
export function piLayout(prefix: string, extensions: Record<string, string>): PiLayout {
  const root = tempDir(prefix);
  const workspace = join(root, "workspace");
  mkdirSync(join(workspace, ".pi", "extensions"), { recursive: true });
  mkdirSync(join(root, "agent"), { recursive: true });
  mkdirSync(join(root, "home"), { recursive: true });
  writeFileSync(join(root, "agent", "settings.json"), JSON.stringify({ quietStartup: true, defaultProjectTrust: "never" }));
  writeFileSync(join(workspace, "AGENTS.md"), "# 记忆\n");
  for (const [name, source] of Object.entries(extensions)) writeFileSync(join(workspace, ".pi", "extensions", name), source);
  return { root, workspace };
}

/** Argument list of the agent process (contracts 2.1) without the session flag. */
export function agentArgs(): string[] {
  return ["--tools", "+grep,+find,+ls", "-e", EXTENSION_DIR];
}

const TOOLS_PROBE = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.registerCommand("bbtools", {
    description: "report the registered tools",
    handler: async (_args, ctx) => {
      ctx.ui.notify(JSON.stringify(pi.getAllTools()), "info");
    },
  });
}
`;

/** ToolInfo of every tool registered in a real pi (trial mode) whose workspace has the given extensions. */
export async function piToolInfos(extensions: Record<string, string>): Promise<ToolInfo[]> {
  const own = piLayout("bbim-tools-", { "tools-probe.ts": TOOLS_PROBE, ...extensions });
  const pi = new RpcProcess(["--no-session", ...agentArgs()], { cwd: own.workspace, env: agentEnv(own.root, own.workspace, { BEARBOT_TRIAL: "1" }) });
  try {
    await pi.request("boot", { type: "get_state" });
    const response = await pi.request("tools", { type: "prompt", message: "/bbtools" });
    if (response.success !== true) throw new Error(`/bbtools 失败：${JSON.stringify(response)}`);
    return JSON.parse(await pi.waitFor(() => pi.notifications()[0], "工具列表")) as ToolInfo[];
  } finally {
    await pi.stop();
  }
}
