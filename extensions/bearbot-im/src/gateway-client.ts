import { EventEmitter } from "node:events";
import { createConnection, type Socket } from "node:net";

// gateway.sock protocol types (contracts section 5).

export type WireBinding = {
  channel: string;
  name: string | null;
  platform: string;
  trigger: "mention" | "all" | "prefix";
  prefix: string | null;
  replyMode: "final" | "each";
  interrupt: "queue" | "steer";
};

export type WireAttachment = {
  type: "image" | "file" | "audio" | "video";
  name: string;
  path?: string;
  size?: number;
  mimeType?: string;
  error?: string;
  data?: string;
};

export type WireMessage = {
  seq: number[];
  kind: "message" | "command" | "action" | "reaction";
  id: string;
  platform: string;
  channel: { id: string; name?: string; isDM: boolean };
  thread: string;
  sender: { id: string; name: string; role: "admin" | "member"; isBot: boolean };
  time: string;
  text: string;
  mentionsBot: boolean;
  edited: boolean;
  stale: boolean;
  replyTo?: { id: string; sender: string; name: string; text: string };
  command?: { name: string; args: string };
  action?: { actionId: string; value?: string };
  reaction?: { emoji: string; added: boolean; messageId: string };
  attachments: WireAttachment[];
  trigger: boolean;
  direction?: "in" | "out";
};

export type ClassifierRule = { pattern: string; action: "allow" | "deny" };
export type ClassifierSettings = { enabled: boolean; rules: ClassifierRule[] };

export type HelloResult = {
  bindings: WireBinding[];
  admins: string[];
  classifier: ClassifierSettings;
  self: Record<string, string>;
};

export type PostParams = {
  target: string;
  markdown?: string;
  files?: { path: string; name?: string }[];
  card?: unknown;
  replyTo?: string;
};

export type TargetsResult = {
  targets: {
    channel: string;
    name: string | null;
    platform: string;
    isDM: boolean;
    threads: { thread: string; lastActiveAt: string }[];
  }[];
};

export type TurnNotification =
  | { active: true; origin: string | null; role: "admin" | "member" | "system"; startedAt: string }
  | { active: false };

/** Interface of the gateway methods used for sending, so callers can depend on it without the socket details. */
export interface GatewaySender {
  post(params: PostParams): Promise<{ ref: string }>;
}

type Pending = { method: string; resolve: (value: unknown) => void; reject: (error: Error) => void };

type JsonRpcIncoming = {
  jsonrpc?: string;
  id?: number | string | null;
  method?: string;
  params?: unknown;
  result?: unknown;
  error?: { code: number; message: string; data?: unknown };
};

export interface GatewayClientEvents {
  hello: [result: HelloResult, reconnect: boolean];
  message: [message: WireMessage];
  bindings: [bindings: WireBinding[]];
  admins: [admins: string[]];
  classifier: [settings: ClassifierSettings];
  disconnected: [];
  problem: [kind: "connect" | "hello", detail: string];
}

export const RECONNECT_DELAY_MS = 1000;
export const SEND_WAIT_MS = 30_000;

/**
 * Newline-delimited JSON-RPC 2.0 client for gateway.sock. Reconnects every second; requests made while the
 * connection is down wait up to 30 seconds for it to come back.
 */
export class GatewayClient extends EventEmitter<GatewayClientEvents> implements GatewaySender {
  private socket: Socket | null = null;
  private helloDone = false;
  private everConnected = false;
  private nextId = 1;
  private readonly pending = new Map<number, Pending>();
  private readonly waiters = new Set<() => void>();
  private reconnectTimer: NodeJS.Timeout | null = null;
  private started = false;
  private closed = false;
  private buffer = "";

  constructor(
    private readonly socketPath: string,
    private readonly ackedSeq: () => number | null,
  ) {
    super();
  }

  get connected(): boolean {
    return this.helloDone;
  }

  start(): void {
    if (this.started) throw new Error("GatewayClient 已经启动");
    this.started = true;
    this.connect();
  }

  close(): void {
    this.closed = true;
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.socket?.destroy();
  }

  async request<T>(method: string, params: unknown, waitMs = SEND_WAIT_MS): Promise<T> {
    if (!this.helloDone) await this.waitConnected(method, waitMs);
    return this.send<T>(method, params);
  }

  /** Sends a notification when connected; returns false when there is no connection. */
  notify(method: string, params: unknown): boolean {
    if (!this.helloDone || !this.socket) return false;
    this.socket.write(`${JSON.stringify({ jsonrpc: "2.0", method, params })}\n`);
    return true;
  }

  post(params: PostParams): Promise<{ ref: string }> {
    return this.request<{ ref: string }>("post", params);
  }

  private waitConnected(method: string, waitMs: number): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const done = () => {
        clearTimeout(timer);
        this.waiters.delete(done);
        resolve();
      };
      const timer = setTimeout(() => {
        this.waiters.delete(done);
        reject(new Error(`gateway 未连接，等待 ${Math.round(waitMs / 1000)} 秒后仍未恢复（${method}）`));
      }, waitMs);
      this.waiters.add(done);
    });
  }

  private send<T>(method: string, params: unknown, socket: Socket | null = this.socket): Promise<T> {
    if (!socket) return Promise.reject(new Error(`gateway 未连接（${method}）`));
    const id = this.nextId++;
    return new Promise<T>((resolve, reject) => {
      this.pending.set(id, { method, resolve: resolve as (value: unknown) => void, reject });
      socket.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`);
    });
  }

  private connect(): void {
    if (this.closed) return;
    const socket = createConnection(this.socketPath);
    this.socket = socket;
    this.buffer = "";
    socket.setEncoding("utf8");
    socket.on("connect", () => {
      const acked = this.ackedSeq();
      const params = acked === null ? {} : { ackedSeq: acked };
      this.send<HelloResult>("hello", params, socket).then(
        (result) => {
          if (this.socket !== socket) return;
          this.helloDone = true;
          const reconnect = this.everConnected;
          this.everConnected = true;
          for (const waiter of [...this.waiters]) waiter();
          this.emit("hello", result, reconnect);
        },
        (error: Error) => {
          // The gateway refuses a second connection; reconnect later (DESIGN 3).
          if (this.socket !== socket) return;
          this.emit("problem", "hello", error.message);
          socket.destroy();
        },
      );
    });
    socket.on("data", (chunk: string) => this.onData(chunk));
    socket.on("error", (error: NodeJS.ErrnoException) => {
      if (!this.helloDone) this.emit("problem", "connect", `${error.code ?? ""} ${error.message}`.trim());
    });
    socket.on("close", () => {
      if (this.socket !== socket) return;
      this.socket = null;
      const wasConnected = this.helloDone;
      this.helloDone = false;
      const failed = [...this.pending.values()];
      this.pending.clear();
      for (const entry of failed) entry.reject(new Error(`gateway 连接断开（${entry.method}）`));
      if (wasConnected) this.emit("disconnected");
      if (!this.closed) this.reconnectTimer = setTimeout(() => this.connect(), RECONNECT_DELAY_MS);
    });
  }

  private onData(chunk: string): void {
    this.buffer += chunk;
    let index = this.buffer.indexOf("\n");
    while (index !== -1) {
      const line = this.buffer.slice(0, index);
      this.buffer = this.buffer.slice(index + 1);
      if (line.trim() !== "") this.onLine(line);
      index = this.buffer.indexOf("\n");
    }
  }

  private onLine(line: string): void {
    const message = JSON.parse(line) as JsonRpcIncoming;
    if (message.method !== undefined && (message.id === undefined || message.id === null)) {
      this.onNotification(message.method, message.params);
      return;
    }
    if (typeof message.id !== "number") throw new Error(`gateway 发来无法识别的记录：${line}`);
    const entry = this.pending.get(message.id);
    if (!entry) throw new Error(`gateway 的响应没有对应的请求：${line}`);
    this.pending.delete(message.id);
    if (message.error) entry.reject(new Error(message.error.message));
    else entry.resolve(message.result);
  }

  private onNotification(method: string, params: unknown): void {
    switch (method) {
      case "message":
        this.emit("message", params as WireMessage);
        return;
      case "bindings.changed":
        this.emit("bindings", (params as { bindings: WireBinding[] }).bindings);
        return;
      case "admins.changed":
        this.emit("admins", (params as { admins: string[] }).admins);
        return;
      case "classifier.changed":
        this.emit("classifier", params as ClassifierSettings);
        return;
      default:
        throw new Error(`gateway 发来未知的通知：${method}`);
    }
  }
}
