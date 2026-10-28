import type { Socket } from "node:net";
import { errorText, type PinoLogger } from "./log.ts";

export const METHOD_NOT_FOUND = -32601;
export const INVALID_PARAMS = -32602;
export const APP_ERROR = -32000;

/** An error answered to the peer; `message` is Chinese text for the user when code is APP_ERROR. */
export class RpcError extends Error {
  constructor(
    readonly code: number,
    message: string,
  ) {
    super(message);
    this.name = "RpcError";
  }
}

export type RequestHandler = (params: unknown, conn: JsonRpcConnection) => unknown;
export type NotificationHandler = (params: unknown, conn: JsonRpcConnection) => void;

type Pending = { resolve: (v: unknown) => void; reject: (e: Error) => void; method: string };

type Incoming = {
  jsonrpc?: unknown;
  id?: unknown;
  method?: unknown;
  params?: unknown;
  result?: unknown;
  error?: { code?: unknown; message?: unknown };
};

/** Newline-delimited JSON-RPC 2.0 on one socket; both sides may send requests (contracts 4 and 5). */
export class JsonRpcConnection {
  private buffer = "";
  private nextId = 1;
  private readonly pending = new Map<string | number, Pending>();
  private closedFlag = false;
  private readonly closeListeners: (() => void)[] = [];

  constructor(
    private readonly socket: Socket,
    private readonly options: {
      name: string;
      logger: PinoLogger;
      /** Prefix of ids of requests sent by this side ("g" on gateway-ctl.sock); numeric ids when absent. */
      idPrefix?: string;
      requests: Record<string, RequestHandler>;
      notifications?: Record<string, NotificationHandler>;
    },
  ) {
    socket.setEncoding("utf8");
    socket.on("data", (chunk: string) => this.onData(chunk));
    socket.on("error", (e) => {
      options.logger.warn({ connection: options.name, err: e }, "connection error");
    });
    socket.on("close", () => this.onClose());
  }

  get closed(): boolean {
    return this.closedFlag;
  }

  onClosed(listener: () => void): void {
    this.closeListeners.push(listener);
  }

  request(method: string, params: unknown): Promise<unknown> {
    if (this.closedFlag) {
      return Promise.reject(new RpcError(APP_ERROR, `${this.options.name} 连接已断开`));
    }
    const n = this.nextId++;
    const id = this.options.idPrefix ? `${this.options.idPrefix}${n}` : n;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject, method });
      this.write({ jsonrpc: "2.0", id, method, params });
    });
  }

  notify(method: string, params: unknown): void {
    if (this.closedFlag) {
      return;
    }
    this.write({ jsonrpc: "2.0", method, params });
  }

  close(): void {
    this.socket.end();
    this.socket.destroySoon();
  }

  destroy(): void {
    this.socket.destroy();
  }

  private write(message: object): void {
    this.socket.write(`${JSON.stringify(message)}\n`);
  }

  private onData(chunk: string): void {
    this.buffer += chunk;
    let nl: number;
    while ((nl = this.buffer.indexOf("\n")) >= 0) {
      const line = this.buffer.slice(0, nl);
      this.buffer = this.buffer.slice(nl + 1);
      if (line.trim() === "") {
        continue;
      }
      let msg: Incoming;
      try {
        msg = JSON.parse(line) as Incoming;
      } catch (e) {
        // A peer that sends broken framing cannot be talked to any more.
        this.options.logger.error({ connection: this.options.name, line, err: e }, "invalid JSON, closing connection");
        this.socket.destroy();
        return;
      }
      this.dispatch(msg);
    }
  }

  private dispatch(msg: Incoming): void {
    if (typeof msg !== "object" || msg === null || Array.isArray(msg)) {
      this.options.logger.error({ connection: this.options.name, msg }, "invalid JSON-RPC message, closing connection");
      this.socket.destroy();
      return;
    }
    if (typeof msg.method === "string") {
      if (msg.id === undefined || msg.id === null) {
        this.handleNotification(msg.method, msg.params);
      } else if (typeof msg.id === "string" || typeof msg.id === "number") {
        this.handleRequest(msg.id, msg.method, msg.params);
      } else {
        this.options.logger.error({ connection: this.options.name, msg }, "invalid request id, closing connection");
        this.socket.destroy();
      }
      return;
    }
    if ((typeof msg.id === "string" || typeof msg.id === "number") && ("result" in msg || "error" in msg)) {
      const p = this.pending.get(msg.id);
      if (!p) {
        this.options.logger.warn({ connection: this.options.name, id: msg.id }, "response to unknown request");
        return;
      }
      this.pending.delete(msg.id);
      if (msg.error) {
        const message = typeof msg.error.message === "string" ? msg.error.message : JSON.stringify(msg.error);
        const code = typeof msg.error.code === "number" ? msg.error.code : APP_ERROR;
        p.reject(new RpcError(code, message));
      } else {
        p.resolve(msg.result);
      }
      return;
    }
    this.options.logger.error({ connection: this.options.name, msg }, "unexpected JSON-RPC message, closing connection");
    this.socket.destroy();
  }

  private handleNotification(method: string, params: unknown): void {
    const handler = this.options.notifications?.[method];
    if (!handler) {
      this.options.logger.warn({ connection: this.options.name, method }, "unknown notification");
      return;
    }
    try {
      handler(params, this);
    } catch (e) {
      // A notification has no response; a malformed one is logged and dropped.
      this.options.logger.error({ connection: this.options.name, method, params, err: e }, "notification rejected");
    }
  }

  private handleRequest(id: string | number, method: string, params: unknown): void {
    const handler = this.options.requests[method];
    if (!handler) {
      this.respondError(id, new RpcError(METHOD_NOT_FOUND, `Method not found: ${method}`));
      return;
    }
    Promise.resolve()
      .then(() => handler(params, this))
      .then(
        (result) => {
          if (!this.closedFlag) {
            this.write({ jsonrpc: "2.0", id, result: result ?? {} });
          }
        },
        (e: unknown) => {
          if (!(e instanceof RpcError)) {
            this.options.logger.error({ connection: this.options.name, method, err: e }, "request failed");
          }
          this.respondError(id, e instanceof RpcError ? e : new RpcError(APP_ERROR, errorText(e)));
        },
      );
  }

  private respondError(id: string | number, e: RpcError): void {
    if (this.closedFlag) {
      return;
    }
    this.write({ jsonrpc: "2.0", id, error: { code: e.code, message: e.message } });
  }

  private onClose(): void {
    if (this.closedFlag) {
      return;
    }
    this.closedFlag = true;
    for (const p of this.pending.values()) {
      p.reject(new RpcError(APP_ERROR, `${this.options.name} 连接已断开，请求 ${p.method} 没有得到响应`));
    }
    this.pending.clear();
    for (const l of this.closeListeners) {
      l();
    }
  }
}

// Parameter readers: wrong types are answered with -32602.

export type Params = Record<string, unknown>;

export function paramsObject(params: unknown): Params {
  if (params === undefined || params === null) {
    return {};
  }
  if (typeof params !== "object" || Array.isArray(params)) {
    throw new RpcError(INVALID_PARAMS, "params must be an object");
  }
  return params as Params;
}

export function reqString(p: Params, key: string): string {
  const v = p[key];
  if (typeof v !== "string" || v === "") {
    throw new RpcError(INVALID_PARAMS, `${key} must be a non-empty string`);
  }
  return v;
}

export function optString(p: Params, key: string): string | undefined {
  const v = p[key];
  if (v === undefined || v === null) {
    return undefined;
  }
  if (typeof v !== "string") {
    throw new RpcError(INVALID_PARAMS, `${key} must be a string`);
  }
  return v;
}

export function nullableString(p: Params, key: string): string | null {
  const v = p[key];
  if (v === null || v === undefined) {
    return null;
  }
  if (typeof v !== "string") {
    throw new RpcError(INVALID_PARAMS, `${key} must be a string or null`);
  }
  return v;
}

export function reqInt(p: Params, key: string, min: number): number {
  const v = p[key];
  if (typeof v !== "number" || !Number.isInteger(v) || v < min) {
    throw new RpcError(INVALID_PARAMS, `${key} must be an integer >= ${min}`);
  }
  return v;
}

export function optInt(p: Params, key: string, min: number): number | undefined {
  if (p[key] === undefined || p[key] === null) {
    return undefined;
  }
  return reqInt(p, key, min);
}

export function optBool(p: Params, key: string): boolean | undefined {
  const v = p[key];
  if (v === undefined || v === null) {
    return undefined;
  }
  if (typeof v !== "boolean") {
    throw new RpcError(INVALID_PARAMS, `${key} must be a boolean`);
  }
  return v;
}
