// Test side of the protocols against the built gateway process: the tests send the requests and notifications
// bearbotd (gateway-ctl.sock, console fds) and bearbot-im (gateway.sock) send. Requests from the gateway to
// bearbotd are only recorded and get no answer; /bb commands that need bearbotd's answer are tested in daemon/e2e
// with the real bearbotd.
import { spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { connect, type Socket } from "node:net";
import { dirname, join } from "node:path";
import type { Readable, Writable } from "node:stream";
import { fileURLToPath } from "node:url";

export const GATEWAY_DIR = join(dirname(fileURLToPath(import.meta.url)), "..");
export const MAIN_JS = join(GATEWAY_DIR, "dist", "main.js");
/** Time zone of the gateway processes; dates the gateway computes (inbox/, console-files/) use it. */
export const GATEWAY_TZ = process.env.TZ ?? "Asia/Shanghai";

/** YYYY-MM-DD of `date` in the gateway's time zone. */
export function gatewayDate(date = new Date()): string {
  return new Intl.DateTimeFormat("en-CA", { timeZone: GATEWAY_TZ, year: "numeric", month: "2-digit", day: "2-digit" }).format(date);
}

export function requireBuild(): void {
  if (!existsSync(MAIN_JS)) {
    throw new Error(`${MAIN_JS} is missing: run "npm run build -w gateway" first`);
  }
}

export type Env = {
  root: string;
  workspace: string;
  dataDir: string;
  consoleFilesDir: string;
  configPath: string;
  gatewaySocket: string;
  ctlSocket: string;
  database: string;
  config: Record<string, unknown>;
};

/** A fresh root under /tmp (unix socket paths must stay short). */
export function makeEnv(overrides: Record<string, unknown> = {}): Env {
  const root = mkdtempSync("/tmp/bbgw");
  const workspace = join(root, "ws");
  const dataDir = join(root, "data");
  mkdirSync(workspace);
  mkdirSync(dataDir);
  const env: Env = {
    root,
    workspace,
    dataDir,
    consoleFilesDir: join(dataDir, "console-files"),
    configPath: join(root, "gateway.json"),
    gatewaySocket: join(root, "gw.sock"),
    ctlSocket: join(root, "ctl.sock"),
    database: join(dataDir, "gateway.db"),
    config: {},
  };
  env.config = {
    logLevel: "debug",
    userName: "bearbot",
    gatewaySocket: env.gatewaySocket,
    ctlSocket: env.ctlSocket,
    socketGroupGid: process.getgid!(),
    database: env.database,
    consoleFilesDir: env.consoleFilesDir,
    workspaceDir: workspace,
    tokenTtlMs: 600000,
    replayWindowMs: 600000,
    messageRetentionMs: 2592000000,
    attachmentTimeoutMs: 60000,
    imageMaxBytes: 5242880,
    maintenanceNotice: "bearbot 正在修复，暂时无法回复。",
    classifierEnabledDefault: true,
    http: { listen: "" },
    platforms: {},
    console: { inputFd: 3, outputFd: 4 },
    ...overrides,
  };
  writeConfig(env);
  return env;
}

export function writeConfig(env: Env): void {
  writeFileSync(env.configPath, JSON.stringify(env.config, null, 2));
}

export function removeEnv(env: Env): void {
  rmSync(env.root, { recursive: true, force: true });
}

export function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

/** Polls `check` until it returns a value other than undefined. */
export async function waitFor<T>(what: string, check: () => T | undefined, timeoutMs = 10_000): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const v = check();
    if (v !== undefined) {
      return v;
    }
    if (Date.now() > deadline) {
      throw new Error(`timed out waiting for ${what}`);
    }
    await sleep(20);
  }
}

// eslint-disable-next-line no-control-regex
const ANSI = /\x1b\[[0-9;?]*[A-Za-z]|\x1b[=>]|\r/g;

export function stripAnsi(s: string): string {
  return s.replace(ANSI, "");
}

/** The gateway process with the console on two pipes (line mode), like bearbotd starts it with `docker run -i`. */
export class GatewayProcess {
  readonly child: ChildProcess;
  consoleOut = "";
  logOut = "";
  exitCode: number | null = null;
  exitSignal: NodeJS.Signals | null = null;
  readonly exited: Promise<void>;

  constructor(
    readonly env: Env,
    options: { console?: boolean } = {},
  ) {
    const withConsole = options.console ?? true;
    this.child = spawn(process.execPath, [MAIN_JS, "--config", env.configPath], {
      cwd: GATEWAY_DIR,
      stdio: withConsole ? ["ignore", "pipe", "pipe", "pipe", "pipe"] : ["ignore", "pipe", "pipe"],
      env: { PATH: process.env.PATH ?? "/usr/bin:/bin", HOME: env.dataDir, TZ: GATEWAY_TZ },
    });
    this.child.stdout!.setEncoding("utf8");
    this.child.stdout!.on("data", (d: string) => (this.logOut += d));
    this.child.stderr!.setEncoding("utf8");
    this.child.stderr!.on("data", (d: string) => (this.logOut += d));
    if (withConsole) {
      const out = this.child.stdio[4] as Readable;
      out.setEncoding("utf8");
      out.on("data", (d: string) => (this.consoleOut += d));
    }
    this.exited = new Promise((resolve) => {
      this.child.on("exit", (code, signal) => {
        this.exitCode = code;
        this.exitSignal = signal;
        resolve();
      });
    });
  }

  get consoleIn(): Writable {
    return this.child.stdio[3] as Writable;
  }

  type(line: string): void {
    this.consoleIn.write(`${line}\n`);
  }

  endInput(): void {
    this.consoleIn.end();
  }

  /** Waits until the console output after `from` matches; returns the match. */
  async waitConsole(pattern: RegExp, from = 0, timeoutMs = 10_000): Promise<RegExpMatchArray> {
    return waitFor(`console output ${pattern} in:\n${this.consoleOut.slice(from)}\n--- log tail ---\n${this.logOut.slice(-3000)}`, () => {
      const m = this.consoleOut.slice(from).match(pattern);
      return m ?? undefined;
    }, timeoutMs);
  }

  async waitLog(pattern: RegExp, timeoutMs = 10_000): Promise<void> {
    await waitFor(`log ${pattern}`, () => (pattern.test(this.logOut) ? true : undefined), timeoutMs);
  }

  async ready(): Promise<void> {
    await this.waitLog(/"msg":"gateway ready"/);
  }

  async stop(): Promise<void> {
    if (this.exitCode === null && this.exitSignal === null) {
      this.child.kill("SIGTERM");
      await this.exited;
    }
  }

  kill(): Promise<void> {
    if (this.exitCode === null && this.exitSignal === null) {
      this.child.kill("SIGKILL");
    }
    return this.exited;
  }
}

export type RpcErrorValue = { code: number; message: string };

export class RpcCallError extends Error {
  constructor(readonly error: RpcErrorValue) {
    super(`${error.code}: ${error.message}`);
  }
}

type Notification = { method: string; params: any };

/**
 * Newline-delimited JSON-RPC 2.0 peer used by the tests. On gateway-ctl.sock it connects like bearbotd (ids
 * "d<n>"); requests from the gateway are recorded in `requests` and answered only when a test calls `respond`. On
 * gateway.sock it connects like bearbot-im (numeric ids).
 */
export class RpcPeer {
  private buffer = "";
  private nextId = 1;
  private readonly pending = new Map<string | number, { resolve: (v: any) => void; reject: (e: Error) => void }>();
  readonly notifications: Notification[] = [];
  readonly requests: { id: string | number; method: string; params: any }[] = [];
  closed = false;
  readonly closedPromise: Promise<void>;

  private constructor(
    private readonly socket: Socket,
    private readonly idPrefix: string,
  ) {
    socket.setEncoding("utf8");
    socket.on("data", (d: string) => this.onData(d));
    this.closedPromise = new Promise((resolve) => {
      socket.on("close", () => {
        this.closed = true;
        for (const p of this.pending.values()) p.reject(new Error("closed"));
        resolve();
      });
    });
    socket.on("error", () => {});
  }

  static connect(path: string, idPrefix = ""): Promise<RpcPeer> {
    return new Promise((resolve, reject) => {
      const socket = connect(path);
      socket.once("error", reject);
      socket.once("connect", () => {
        socket.off("error", reject);
        resolve(new RpcPeer(socket, idPrefix));
      });
    });
  }

  request(method: string, params: unknown = {}): Promise<any> {
    const n = this.nextId++;
    const id = this.idPrefix ? `${this.idPrefix}${n}` : n;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.socket.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`);
    });
  }

  notify(method: string, params: unknown): void {
    this.socket.write(`${JSON.stringify({ jsonrpc: "2.0", method, params })}\n`);
  }

  async requestError(method: string, params: unknown = {}): Promise<RpcErrorValue> {
    try {
      const result = await this.request(method, params);
      throw new Error(`expected an error from ${method}, got ${JSON.stringify(result)}`);
    } catch (e) {
      if (e instanceof RpcCallError) {
        return e.error;
      }
      throw e;
    }
  }

  messages(): any[] {
    return this.notifications.filter((n) => n.method === "message").map((n) => n.params);
  }

  async waitNotification(method: string, predicate: (params: any) => boolean = () => true, timeoutMs = 10_000): Promise<any> {
    return waitFor(
      `notification ${method}; got ${JSON.stringify(this.notifications.map((n) => n.method))}`,
      () => this.notifications.find((n) => n.method === method && predicate(n.params))?.params,
      timeoutMs,
    );
  }

  async waitMessage(predicate: (m: any) => boolean, timeoutMs = 10_000): Promise<any> {
    return this.waitNotification("message", predicate, timeoutMs);
  }

  async waitRequest(method: string, timeoutMs = 10_000): Promise<any> {
    return waitFor(`request ${method}`, () => this.requests.find((r) => r.method === method)?.params, timeoutMs);
  }

  /** The `n`th request (from 0) of `method` from the gateway, with the id to answer it with `respond`. */
  async waitRequestEntry(method: string, n = 0, timeoutMs = 10_000): Promise<{ id: string | number; params: any }> {
    return waitFor(`request ${method} #${n}`, () => this.requests.filter((r) => r.method === method)[n], timeoutMs);
  }

  /** Answers a request from the gateway with a result or with a -32000 error. */
  respond(id: string | number, answer: { result: unknown } | { error: string }): void {
    const body = "result" in answer ? { result: answer.result } : { error: { code: -32000, message: answer.error } };
    this.socket.write(`${JSON.stringify({ jsonrpc: "2.0", id, ...body })}\n`);
  }

  close(): Promise<void> {
    this.socket.end();
    return this.closedPromise;
  }

  private onData(chunk: string): void {
    this.buffer += chunk;
    let nl: number;
    while ((nl = this.buffer.indexOf("\n")) >= 0) {
      const line = this.buffer.slice(0, nl);
      this.buffer = this.buffer.slice(nl + 1);
      const msg = JSON.parse(line);
      if (typeof msg.method === "string" && msg.id !== undefined) {
        this.requests.push({ id: msg.id, method: msg.method, params: msg.params });
      } else if (typeof msg.method === "string") {
        this.notifications.push({ method: msg.method, params: msg.params });
      } else {
        const p = this.pending.get(msg.id);
        if (!p) throw new Error(`response to unknown id ${msg.id}`);
        this.pending.delete(msg.id);
        if (msg.error) p.reject(new RpcCallError(msg.error));
        else p.resolve(msg.result);
      }
    }
  }
}

/** Connects as bearbotd: hello, then agent.state like the daemon does after every hello. */
export async function connectDaemon(env: Env, state = "running"): Promise<RpcPeer> {
  const ctl = await RpcPeer.connect(env.ctlSocket, "d");
  await ctl.request("hello", {});
  await ctl.request("agent.state", { state, reason: null });
  return ctl;
}

/** Connects as bearbot-im and sends hello. */
export async function connectBridge(env: Env, ackedSeq?: number): Promise<{ peer: RpcPeer; hello: any }> {
  const peer = await RpcPeer.connect(env.gatewaySocket);
  const hello = await peer.request("hello", ackedSeq === undefined ? {} : { ackedSeq });
  return { peer, hello };
}

export const CLOCK = "\\d{2}:\\d{2}:\\d{2}";
