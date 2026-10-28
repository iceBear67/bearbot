import { type ChildProcess, execFile, spawn } from "node:child_process";
import {
  closeSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
  writeSync,
} from "node:fs";
import { request } from "node:http";
import { connect } from "node:net";
import { dirname, join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { promisify } from "node:util";
import { REPO } from "./helpers.ts";

// The rootless test root of contracts section 10 with the real bearbotd, gateway and pi, for tests that send
// messages through gateway -> bearbot-im -> pi. It follows daemon/internal/testenv.

const run = promisify(execFile);

/** `go build -o <dir>/bearbotd ./cmd/bearbotd` in the daemon module. */
export async function buildDaemon(dir: string): Promise<string> {
  const out = join(dir, "bearbotd");
  await run("go", ["build", "-o", out, "./cmd/bearbotd"], { cwd: join(REPO, "daemon"), timeout: 600_000 });
  return out;
}

// Path under R -> target; targets starting with "@" are relative to the repository.
const SYMLINKS: [string, string][] = [
  ["/opt/bearbot/gateway", "@gateway"],
  ["/opt/bearbot/extensions", "@extensions"],
  ["/opt/bearbot/sdk", "@sdk"],
  ["/opt/bearbot/skills", "@skills"],
  ["/opt/bearbot/recovery-profile", "@recovery-profile"],
  ["/opt/bearbot/toolchain", "@"],
  ["/usr/local/lib/node_modules/@earendil-works/pi-coding-agent", "@node_modules/@earendil-works/pi-coding-agent"],
  ["/usr/local/bin/pi", "@node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js"],
  ["/usr/local/bin/python3.14", "/usr/bin/python3.14"],
  ["/usr/local/bin/uv", "/usr/bin/uv"],
  ["/usr/local/bin/uvx", "/usr/bin/uvx"],
];

/** The test values of daemon/e2e (testenv.DefaultConfig with the short timeouts of fastConfig). */
function config(errorNotice: string): string {
  const q = (value: string) => JSON.stringify(value);
  return `[daemon]
log_level = "debug"
shutdown_grace = "5s"
term_grace = "2s"

[agent]
start_timeout = "60s"
ready_timeout = "40s"
heartbeat_interval = "2s"
rpc_timeout = "5s"
crash_limit = 3
crash_window = "1m"
probation = "4s"
drain_timeout = "30s"

[agent.env]

[gateway]
user_name = "bearbot"
crash_limit = 5
token_ttl = "10m"
replay_window = "10m"
message_retention = "30d"
attachment_timeout = "60s"
image_max_bytes = 5242880
maintenance_notice = "bearbot 正在修复，暂时无法回复。"
[gateway.http]
listen = ""

[platforms.telegram]
enabled = false
bot_token = ""

[platforms.qq]
enabled = false
app_id = ""
client_secret = ""
sandbox = false

[platforms.wecom]
enabled = false

[platforms.console]
enabled = true

[bridge]
observe_limit = 30
hold_warn_after = "10m"
error_notice = ${q(errorNotice)}

[classifier]
enabled = true
model = "anthropic/claude-haiku-4-5-20251001"
timeout = "15s"
rules_max_bytes = 32768

[recovery]
timeout = "1m"
attempts = 2
[recovery.env]

[maintenance]
on_enter = []
on_exit = []
hook_timeout = "10s"
`;
}

export const ERROR_NOTICE = "处理时出现错误，已通知管理员。";

/** A root prefix R under /tmp (unix socket paths must stay short). */
export class TestRoot {
  readonly root: string;

  constructor() {
    const gatewayMain = join(REPO, "gateway", "dist", "main.js");
    if (!existsSync(gatewayMain)) throw new Error(`${gatewayMain} 不存在：请先在仓库根目录执行 npm run build -w gateway`);
    this.root = mkdtempSync("/tmp/bb");
    for (const [link, target] of SYMLINKS) {
      const resolved = target.startsWith("@") ? join(REPO, target.slice(1)) : target;
      if (!existsSync(resolved)) throw new Error(`测试环境需要 ${resolved}`);
      mkdirSync(dirname(this.path(link)), { recursive: true });
      symlinkSync(resolved, this.path(link));
    }
    for (const dir of ["/bearbot/profile", "/bearbot/workspace", "/bearbot/config", "/home/bearbot"]) mkdirSync(this.path(dir), { recursive: true });
    writeFileSync(this.path("/bearbot/config/bearbot.toml"), config(ERROR_NOTICE), { mode: 0o600 });
  }

  path(rel: string): string {
    return join(this.root, rel);
  }

  get workspace(): string {
    return this.path("/bearbot/workspace");
  }

  get agentSock(): string {
    return this.path("/run/bearbot/agent.sock");
  }

  get classifierMd(): string {
    return join(this.workspace, "CLASSIFIER.md");
  }

  get diagnosticsFile(): string {
    return join(this.workspace, ".bearbot", "diagnostics.jsonl");
  }

  get stateFile(): string {
    return this.path("/bearbot/profile/bearbot/daemon/state.json");
  }

  get daemonLog(): string {
    return this.path("/bearbot/profile/bearbot/logs/daemon.log");
  }

  get gatewayLog(): string {
    return this.path("/bearbot/profile/bearbot/logs/gateway.log");
  }

  get agentStderrLog(): string {
    return this.path("/bearbot/profile/bearbot/logs/agent/stderr.log");
  }

  get agentExitsLog(): string {
    return this.path("/bearbot/profile/bearbot/logs/agent/exits.jsonl");
  }

  get agentRpcEventsLog(): string {
    return this.path("/bearbot/profile/bearbot/logs/agent/rpc-events.jsonl");
  }

  get classifierLog(): string {
    return this.path("/bearbot/profile/bearbot/logs/agent/classifier.jsonl");
  }

  /** Writes a file under the workspace, creating parent directories. */
  writeWorkspaceFile(rel: string, content: string): string {
    const file = join(this.workspace, rel);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, content);
    return file;
  }

  /** Writes a file under the agent profile (PI_CODING_AGENT_DIR), creating parent directories. */
  writeAgentProfileFile(rel: string, content: string): string {
    const file = join(this.path("/bearbot/profile/agent"), rel);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, content);
    return file;
  }

  readLog(file: string): string {
    return existsSync(file) ? readFileSync(file, "utf8") : "";
  }

  /** Records of the bearbot-im diagnostics file. */
  diagnostics(): { source: string; level: string; message: string; detail?: string }[] {
    return this.readLog(this.diagnosticsFile).split("\n").filter((line) => line !== "").map((line) => JSON.parse(line));
  }

  /** The gateway's bridge_cursor and the newest inbound seq of message_log. */
  cursor(): { acked: number; newestInbound: number } {
    const db = new DatabaseSync(this.path("/bearbot/profile/bearbot/gateway/gateway.db"), { readOnly: true });
    try {
      const acked = db.prepare("select acked_seq as seq from bridge_cursor where id = 1").get() as { seq: number } | undefined;
      const newest = db.prepare("select max(seq) as seq from message_log where direction = 'in'").get() as { seq: number | null };
      return { acked: acked?.seq ?? 0, newestInbound: newest.seq ?? 0 };
    } finally {
      db.close();
    }
  }

  remove(): void {
    rmSync(this.root, { recursive: true, force: true });
  }
}

export interface AgentAnswer<T> {
  status: number;
  body: T;
}

/** A JSON request to agent.sock. */
export function callAgent<T>(socket: string, method: "GET" | "POST", path: string, body?: unknown): Promise<AgentAnswer<T>> {
  const payload = body === undefined ? undefined : JSON.stringify(body);
  return new Promise((resolve, reject) => {
    const req = request(
      {
        socketPath: socket,
        method,
        path,
        headers: payload === undefined ? {} : { "content-type": "application/json", "content-length": Buffer.byteLength(payload) },
      },
      (res) => {
        let text = "";
        res.setEncoding("utf8");
        res.on("data", (chunk: string) => {
          text += chunk;
        });
        res.on("end", () => resolve({ status: res.statusCode ?? 0, body: JSON.parse(text) as T }));
        res.on("error", reject);
      },
    );
    req.on("error", reject);
    if (payload !== undefined) req.write(payload);
    req.end();
  });
}

function socketAccepts(path: string): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = connect(path);
    socket.on("connect", () => {
      socket.destroy();
      resolve(true);
    });
    socket.on("error", () => resolve(false));
  });
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/** Polls cond every 200 ms until it returns true. */
export async function waitFor(what: string, timeoutMs: number, cond: () => boolean | Promise<boolean>): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!(await cond())) {
    if (Date.now() > deadline) throw new Error(`${what}：${timeoutMs / 1000} 秒内没有达到`);
    await sleep(200);
  }
}

export type AgentStatus = { state: string; pid: number | null };

/** Every process whose parent chain leads to pid. */
function descendants(pid: number): number[] {
  const parents = new Map<number, number>();
  for (const name of readdirSync("/proc")) {
    if (!/^\d+$/.test(name)) continue;
    let stat: string;
    try {
      stat = readFileSync(`/proc/${name}/stat`, "utf8");
    } catch (error) {
      // The process exited while the list was read.
      if ((error as NodeJS.ErrnoException).code === "ENOENT" || (error as NodeJS.ErrnoException).code === "ESRCH") continue;
      throw error;
    }
    parents.set(Number(name), Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1]));
  }
  const found: number[] = [];
  const queue = [pid];
  while (queue.length > 0) {
    const parent = queue.shift() as number;
    for (const [child, ppid] of parents) {
      if (ppid === parent) {
        found.push(child);
        queue.push(child);
      }
    }
  }
  return found;
}

/**
 * A running `bearbotd --root R --rootless run` in console line mode. Node's "pipe" stdio is a socket, which the
 * daemon does not take as console input (DESIGN 5.13), so stdin is a FIFO and stdout a regular file.
 */
export class Daemon {
  readonly child: ChildProcess;
  stderr = "";
  readonly exited: Promise<{ code: number | null; signal: NodeJS.Signals | null }>;
  private stopped = false;

  private constructor(
    readonly bin: string,
    readonly env: TestRoot,
    private readonly input: number,
    private readonly output: string,
    stdin: number,
    stdout: number,
  ) {
    this.child = spawn(bin, ["--root", env.root, "--rootless", "run"], { cwd: env.root, env: process.env, stdio: [stdin, stdout, "pipe"] });
    this.child.stderr?.setEncoding("utf8");
    this.child.stderr?.on("data", (chunk: string) => {
      this.stderr += chunk;
    });
    this.exited = new Promise((resolve) => {
      this.child.on("exit", (code, signal) => resolve({ code, signal }));
    });
  }

  static async start(bin: string, env: TestRoot): Promise<Daemon> {
    const fifo = env.path("/console.in");
    const output = env.path("/console.out");
    await run("mkfifo", [fifo]);
    // Opened for reading and writing, so neither open blocks; the test writes through its own descriptor.
    const stdin = openSync(fifo, "r+");
    const input = openSync(fifo, "w");
    const stdout = openSync(output, "w");
    const daemon = new Daemon(bin, env, input, output, stdin, stdout);
    closeSync(stdin);
    closeSync(stdout);
    return daemon;
  }

  /** Console output lines written so far (complete lines only). */
  get lines(): string[] {
    const text = readFileSync(this.output, "utf8");
    return text.slice(0, text.lastIndexOf("\n") + 1).split("\n").slice(0, -1);
  }

  /** Writes one console input line. */
  send(line: string): void {
    writeSync(this.input, `${line}\n`);
  }

  /** Console output lines from index `from` on that contain `text`. */
  linesWith(text: string, from = 0): string[] {
    return this.lines.slice(from).filter((line) => line.includes(text));
  }

  /** Waits for a console output line from index `from` on that contains `text` and returns its index. */
  async waitLine(text: string, timeoutMs: number, from = 0): Promise<number> {
    const found = { index: -1 };
    await waitFor(`控制台输出“${text}”`, timeoutMs, () => {
      if (this.child.exitCode !== null) throw new Error(`bearbotd 已退出（${this.child.exitCode}）：${this.stderr}`);
      found.index = this.lines.findIndex((line, position) => position >= from && line.includes(text));
      return found.index >= 0;
    }).catch((error: Error) => {
      throw new Error(`${error.message}\n控制台输出：\n${this.lines.join("\n")}\nstderr：\n${this.stderr}`);
    });
    return found.index;
  }

  async status(): Promise<AgentAnswer<AgentStatus & { error?: string }>> {
    return callAgent<AgentStatus & { error?: string }>(this.env.agentSock, "GET", "/v1/status");
  }

  /** Waits until GET /v1/status reports `running` (and, when given, a PID other than `notPid`). */
  async waitRunning(timeoutMs: number, notPid: number | null = null): Promise<AgentStatus> {
    const seen: { status: AgentStatus | null } = { status: null };
    await waitFor("agent 进入 running", timeoutMs, async () => {
      if (this.child.exitCode !== null) throw new Error(`bearbotd 已退出（${this.child.exitCode}）：${this.stderr}`);
      if (!(await socketAccepts(this.env.agentSock))) return false;
      const answer = await this.status();
      if (answer.status !== 200) return false;
      seen.status = answer.body;
      return answer.body.state === "running" && answer.body.pid !== null && answer.body.pid !== notPid;
    });
    if (!seen.status) throw new Error("没有取得 agent 状态");
    return seen.status;
  }

  /** Waits until bearbot-im of the current agent process called bridge/ready `count` times since the daemon started. */
  async waitReady(count: number, timeoutMs: number): Promise<void> {
    await waitFor(`第 ${count} 次 bridge/ready`, timeoutMs, () => this.env.readLog(this.env.daemonLog).split("\n").filter((line) => line.includes(" msg=bridge/ready ")).length >= count);
  }

  /** Runs a management subcommand of the same binary. */
  async cli(...args: string[]): Promise<string> {
    const { stdout } = await run(this.bin, ["--root", this.env.root, "--rootless", ...args], { timeout: 180_000 });
    return stdout;
  }

  /** SIGTERM, then up to 60 seconds for the shutdown; kills the remaining process tree when it takes longer. */
  async stop(): Promise<void> {
    if (this.stopped) return;
    this.stopped = true;
    closeSync(this.input);
    if (this.child.exitCode !== null || this.child.signalCode !== null) return;
    const pid = this.child.pid as number;
    this.child.kill("SIGTERM");
    const timer = new Promise<"timeout">((resolve) => setTimeout(() => resolve("timeout"), 60_000));
    if ((await Promise.race([this.exited, timer])) !== "timeout") return;
    for (const child of [...descendants(pid), pid]) {
      try {
        process.kill(child, "SIGKILL");
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
      }
    }
    await this.exited;
    throw new Error(`bearbotd 在 SIGTERM 之后 60 秒内没有退出\nstderr：\n${this.stderr}`);
  }
}
