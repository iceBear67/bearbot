import { request as httpRequest, type IncomingMessage } from "node:http";

// agent.sock API types (contracts section 6).

export type AgentWireState =
  | "stopped" | "starting" | "running" | "draining" | "restarting"
  | "incident" | "repairing" | "maintenance" | "rolling_back";

export type ApplyOrigin = { thread: string | null; requester: string | null };

export type IncidentNotice = {
  id: string;
  trigger: string;
  summary: string;
  changedFiles: string[];
  lesson: string;
  origin: ApplyOrigin;
};

export type RollbackRecord = {
  id: number;
  target: string;
  saved: string;
  reverted: { ref: string; summary: string }[];
  requesters: string[] | null;
  thread: string | null;
  notified: boolean;
};

export type ReadyResult = {
  lastExit?: { code: number | null; signal: string | null; stderrTail: string; at: string };
  applied?: { snapshot: string; summary: string; origin: ApplyOrigin };
  incident?: IncidentNotice;
  rollback?: RollbackRecord;
  classifierRestore?: { restored: boolean; detail: string };
};

export type BridgeEvent =
  | { type: "extension_error"; extensionPath: string; event: string; error: string }
  | { type: "state"; state: AgentWireState; reason: string | null }
  | { type: "incident"; incident: IncidentNotice };

export type ClassifierDecision = {
  time: string;
  turnId: string;
  tool: string;
  argsSummary: string;
  decision: "allow" | "deny";
  reason: string;
  basis: "admin-rule" | "builtin" | "model" | "cache" | "classifier-error" | "rules-file";
  durationMs: number;
};

export type ClassifierTurnStart = { exists: boolean; regular: boolean; size: number; content?: string };
export type ClassifierTurnEnd = { restored: boolean; detail?: string };

export type CheckReport = {
  id: string;
  passed: boolean;
  location: { passed: boolean; violations: string[]; message: string };
  typecheck: {
    ran: boolean;
    passed: boolean;
    errors: { file: string | null; line: number | null; column: number | null; code: string; message: string }[];
    output: string;
  };
  trial: {
    ran: boolean;
    passed: boolean;
    exitCode: number | null;
    signal: string | null;
    timedOut: boolean;
    stderr: string;
    extensionErrors: { extensionPath: string; event: string; error: string }[];
    warnings: string[];
  };
  reportPath: string;
};

export type ApplyResult = { accepted: true; snapshot: string } | { accepted: false; report: CheckReport };

export type StatusInfo = {
  state: AgentWireState;
  reason: string | null;
  pid: number | null;
  startedAt: string | null;
  restarts: number;
  crashesInWindow: number;
  probationUntil: string | null;
  sessionId: string | null;
  runningSnapshot: string | null;
  lastGood: string | null;
  pendingApply: { snapshot: string; summary: string } | null;
  incident: { id: string; trigger: string; attempts: number } | null;
  maintenance: { reason: string; incidentId: string | null; since: string } | null;
  rollback: { id: number; target: string; notified: boolean } | null;
  console: boolean;
};

export type SnapshotParams = { action: "list" | "diff" | "restore"; ref?: string; ref2?: string; paths?: string[] };
export type SnapshotResult = { text: string; restored?: string[] };

export class AgentApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

const EVENTS_RETRY_MS = 1000;

/** HTTP/JSON client for agent.sock (unix socket). */
export class AgentClient {
  constructor(private readonly socketPath: string) {}

  request<T>(method: "GET" | "POST", path: string, body?: unknown): Promise<T> {
    const payload = body === undefined ? undefined : JSON.stringify(body);
    return new Promise<T>((resolve, reject) => {
      const req = httpRequest(
        {
          socketPath: this.socketPath,
          method,
          path,
          headers: payload === undefined
            ? {}
            : { "content-type": "application/json", "content-length": Buffer.byteLength(payload) },
        },
        (res) => {
          readBody(res).then((text) => {
            const status = res.statusCode ?? 0;
            const parsed = text === "" ? undefined : (JSON.parse(text) as unknown);
            if (status >= 400) {
              const message = (parsed as { error?: string } | undefined)?.error ?? `HTTP ${status}`;
              reject(new AgentApiError(status, `daemon 拒绝了 ${method} ${path}：${message}`));
              return;
            }
            resolve(parsed as T);
          }, reject);
        },
      );
      req.on("error", (error) => reject(new Error(`无法访问 agent.sock（${method} ${path}）：${error.message}`)));
      if (payload !== undefined) req.write(payload);
      req.end();
    });
  }

  ready(tools: string[], commands: string[]): Promise<ReadyResult> {
    return this.request<ReadyResult>("POST", "/v1/bridge/ready", { tools, commands });
  }

  status(): Promise<StatusInfo> {
    return this.request<StatusInfo>("GET", "/v1/status");
  }

  async logs(kind: "stderr" | "rpc" | "exits", n: number): Promise<string[]> {
    const result = await this.request<{ lines: string[] }>("GET", `/v1/logs?source=agent&kind=${kind}&n=${n}`);
    return result.lines;
  }

  check(): Promise<CheckReport> {
    return this.request<CheckReport>("POST", "/v1/check", {});
  }

  apply(summary: string, origin: ApplyOrigin): Promise<ApplyResult> {
    return this.request<ApplyResult>("POST", "/v1/apply", { summary, origin });
  }

  snapshot(params: SnapshotParams): Promise<SnapshotResult> {
    return this.request<SnapshotResult>("POST", "/v1/snapshot", params);
  }

  classifierTurnStart(turnId: string, senders: string[]): Promise<ClassifierTurnStart> {
    return this.request<ClassifierTurnStart>("POST", "/v1/classifier/turn", { phase: "start", turnId, senders });
  }

  classifierTurnEnd(turnId: string): Promise<ClassifierTurnEnd> {
    return this.request<ClassifierTurnEnd>("POST", "/v1/classifier/turn", { phase: "end", turnId });
  }

  async classifierDecision(decision: ClassifierDecision): Promise<void> {
    await this.request<unknown>("POST", "/v1/classifier/decisions", decision);
  }

  async rollbackAck(id: number): Promise<void> {
    await this.request<unknown>("POST", "/v1/rollback/ack", { id });
  }

  /**
   * Keeps GET /v1/bridge/events open, reopening it one second after it closes. The returned promise resolves once
   * the first stream is open.
   */
  openEvents(onEvent: (event: BridgeEvent) => void, onProblem: (detail: string) => void): Promise<void> {
    let firstOpen: (() => void) | null = null;
    const opened = new Promise<void>((resolve) => {
      firstOpen = resolve;
    });
    const open = () => {
      const req = httpRequest({ socketPath: this.socketPath, method: "GET", path: "/v1/bridge/events" }, (res) => {
        const status = res.statusCode ?? 0;
        if (status !== 200) {
          readBody(res).then(
            (text) => onProblem(`GET /v1/bridge/events 返回 ${status}：${text}`),
            (error: Error) => onProblem(`GET /v1/bridge/events 返回 ${status}：${error.message}`),
          );
          return;
        }
        if (firstOpen) {
          firstOpen();
          firstOpen = null;
        }
        res.setEncoding("utf8");
        let buffer = "";
        res.on("data", (chunk: string) => {
          buffer += chunk;
          let index = buffer.indexOf("\n");
          while (index !== -1) {
            const line = buffer.slice(0, index);
            buffer = buffer.slice(index + 1);
            if (line.trim() !== "") onEvent(JSON.parse(line) as BridgeEvent);
            index = buffer.indexOf("\n");
          }
        });
      });
      req.on("error", (error) => onProblem(`GET /v1/bridge/events 失败：${error.message}`));
      req.on("close", () => {
        setTimeout(open, EVENTS_RETRY_MS);
      });
      req.end();
    };
    open();
    return opened;
  }
}

function readBody(res: IncomingMessage): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    let text = "";
    res.setEncoding("utf8");
    res.on("data", (chunk: string) => {
      text += chunk;
    });
    res.on("end", () => resolve(text));
    res.on("error", reject);
  });
}
