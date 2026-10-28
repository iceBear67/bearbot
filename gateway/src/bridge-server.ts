import { closeSync, constants, readFileSync, realpathSync } from "node:fs";
import { createServer, type Server, type Socket } from "node:net";
import { join } from "node:path";
import { isCardElement, type CardElement } from "chat";
import type { AgentState } from "./agent-state.ts";
import type { BtwAsker, BtwParams } from "./btw.ts";
import type { ChangeNotifier } from "./commands.ts";
import type { GatewayConfig } from "./config.ts";
import type { Delivery } from "./ingest.ts";
import { errorText, type PinoLogger } from "./log.ts";
import type { InboundRow, MessageLog, MessageRow } from "./message-log.ts";
import { OutboundError, parseRef, type LoadedFile, type Outbound } from "./outbound.ts";
import type { Platforms } from "./platforms.ts";
import type { Repo } from "./repo.ts";
import {
  APP_ERROR,
  INVALID_PARAMS,
  JsonRpcConnection,
  RpcError,
  optBool,
  optInt,
  optString,
  paramsObject,
  reqInt,
  reqString,
  type Params,
  type RequestHandler,
} from "./rpc.ts";
import { openNoFollow, type OpenedFile } from "./safe-open.ts";
import { closeServer, listenUnix } from "./sockets.ts";
import { isoLocal } from "./time.ts";
import type { InboundPayload, OutboundPayload, TurnInfo, WireAttachment, WireMessage } from "./types.ts";

const PUMP_BATCH = 200;
const TARGET_THREADS = 10;
const INBOX_FILE_REPLACED = "文件已被替换，没有读取";

type Active = { conn: JsonRpcConnection; sentSeq: number };

/** gateway.sock (DESIGN 6.8, contracts 5): bearbot-im <-> gateway. */
export class BridgeServer implements Delivery, ChangeNotifier, BtwAsker {
  private readonly server: Server;
  private readonly connections = new Set<JsonRpcConnection>();
  private active: Active | null = null;

  constructor(
    private readonly deps: {
      cfg: GatewayConfig;
      repo: Repo;
      log: MessageLog;
      outbound: Outbound;
      platforms: Platforms;
      agentState: AgentState;
      logger: PinoLogger;
    },
  ) {
    this.server = createServer((socket) => this.accept(socket));
  }

  async listen(): Promise<void> {
    const { cfg, logger } = this.deps;
    await listenUnix(this.server, cfg.gatewaySocket, 0o660, cfg.socketGroupGid);
    logger.info({ socket: cfg.gatewaySocket }, "gateway.sock listening");
  }

  async close(): Promise<void> {
    for (const c of this.connections) c.destroy();
    await closeServer(this.server);
  }

  // ---- notifications ----

  bindingsChanged(): void {
    this.active?.conn.notify("bindings.changed", { bindings: this.deps.repo.wireBindings() });
  }

  adminsChanged(): void {
    this.active?.conn.notify("admins.changed", { admins: this.deps.repo.adminIdentities() });
  }

  classifierChanged(): void {
    this.active?.conn.notify("classifier.changed", this.deps.repo.classifier());
  }

  // ---- requests ----

  askBtw(params: BtwParams): Promise<unknown> | null {
    const active = this.active;
    if (!active || active.conn.closed) {
      return null;
    }
    return active.conn.request("btw", params);
  }

  /**
   * Delivery rule (contracts 5.3): while the agent is running and bearbot-im is connected, send every
   * inbound row after sentSeq in seq order, stopping at the first row whose attachments are not finished.
   */
  pump(): void {
    const { agentState, log, cfg } = this.deps;
    const active = this.active;
    if (!active || active.conn.closed || agentState.state !== "running") {
      return;
    }
    for (;;) {
      const rows = log.inboundAfter(active.sentSeq, PUMP_BATCH);
      for (const row of rows) {
        if (row.payload.pending) {
          return;
        }
        active.conn.notify("message", this.wireInbound(row, Date.now() - cfg.replayWindowMs));
        active.sentSeq = row.seq;
      }
      if (rows.length < PUMP_BATCH) {
        return;
      }
    }
  }

  // ---- connections ----

  private accept(socket: Socket): void {
    const conn = new JsonRpcConnection(socket, {
      name: "gateway.sock",
      logger: this.deps.logger,
      requests: this.handlers(),
      notifications: { turn: (params, c) => this.onTurn(params, c) },
    });
    this.connections.add(conn);
    conn.onClosed(() => {
      this.connections.delete(conn);
      if (this.active?.conn === conn) {
        this.active = null;
        this.deps.agentState.setTurn({ active: false });
        this.deps.logger.info("bearbot-im disconnected");
      }
    });
  }

  private requireActive(conn: JsonRpcConnection): void {
    if (this.active?.conn !== conn) {
      throw new RpcError(APP_ERROR, "请先发送 hello");
    }
  }

  /** The channel of a thread or channel id, or null when the id does not belong to an enabled platform. */
  private channelOf(target: string): string | null {
    const adapter = this.deps.platforms.adapterFor(target);
    if (!adapter) {
      return null;
    }
    try {
      return adapter.channelIdFromThreadId(target);
    } catch (e) {
      this.deps.logger.debug({ target, err: e }, "target is not a valid thread id");
      return null;
    }
  }

  /** Target rule (contracts 5.1): a bound channel, or some admin's notify_thread. */
  private checkTarget(target: string): void {
    const channel = this.channelOf(target);
    if (channel !== null && this.deps.repo.binding(channel)) {
      return;
    }
    if (this.deps.repo.notifyThreads().includes(target)) {
      return;
    }
    throw new RpcError(APP_ERROR, `目标 ${target} 不是已绑定的频道，也不是管理员的通知会话串`);
  }

  private async send<T>(fn: () => Promise<T>): Promise<T> {
    try {
      return await fn();
    } catch (e) {
      if (e instanceof RpcError) throw e;
      if (e instanceof OutboundError) throw new RpcError(APP_ERROR, e.message);
      // DESIGN 6.2.2: a platform send failure goes back to bearbot-im, which reports it as a diagnostic.
      this.deps.logger.warn({ err: e }, "platform send failed");
      throw new RpcError(APP_ERROR, `发送失败：${errorText(e)}`);
    }
  }

  private onTurn(params: unknown, conn: JsonRpcConnection): void {
    if (this.active?.conn !== conn) {
      return;
    }
    const p = paramsObject(params);
    let turn: TurnInfo;
    if (p.active === true) {
      const role = p.role;
      if (role !== "admin" && role !== "member" && role !== "system") {
        this.deps.logger.warn({ params }, "turn notification with an invalid role");
        return;
      }
      turn = {
        active: true,
        origin: typeof p.origin === "string" ? p.origin : null,
        role,
        startedAt: typeof p.startedAt === "string" ? p.startedAt : new Date().toISOString(),
      };
    } else {
      turn = { active: false };
    }
    this.deps.agentState.setTurn(turn);
  }

  private handlers(): Record<string, RequestHandler> {
    const { repo, outbound, platforms, logger } = this.deps;
    return {
      hello: (params, conn) => {
        if (this.active && this.active.conn !== conn) {
          logger.warn("hello rejected: another bearbot-im connection is active");
          throw new RpcError(APP_ERROR, "已有 bearbot-im 连接，同一时间只接受一条连接");
        }
        if (this.active) {
          throw new RpcError(APP_ERROR, "这条连接已经发送过 hello");
        }
        const ackedSeq = optInt(paramsObject(params), "ackedSeq", 0) ?? 0;
        const sentSeq = repo.ack(ackedSeq);
        this.active = { conn, sentSeq };
        logger.info({ ackedSeq, sentSeq }, "bearbot-im connected");
        // Messages follow the hello response.
        setImmediate(() => this.pump());
        return {
          bindings: repo.wireBindings(),
          admins: repo.adminIdentities(),
          classifier: repo.classifier(),
          self: platforms.selfNames(),
        };
      },

      ack: (params, conn) => {
        this.requireActive(conn);
        repo.ack(reqInt(paramsObject(params), "seq", 0));
        return {};
      },

      post: async (params, conn) => {
        this.requireActive(conn);
        const p = paramsObject(params);
        const target = reqString(p, "target");
        this.checkTarget(target);
        const markdown = optString(p, "markdown");
        const card = this.cardParam(p);
        const replyTo = optString(p, "replyTo");
        const files = this.filesParam(p);
        if ((markdown === undefined || markdown === "") && files.length === 0 && card === undefined) {
          throw new RpcError(INVALID_PARAMS, "post needs markdown, files or card");
        }
        const ref = await this.send(() =>
          outbound.post(target, {
            ...(markdown !== undefined ? { markdown } : {}),
            ...(card !== undefined ? { card } : {}),
            ...(replyTo !== undefined ? { replyTo } : {}),
            files,
          }),
        );
        return { ref };
      },

      edit: async (params, conn) => {
        this.requireActive(conn);
        const p = paramsObject(params);
        const ref = this.refParam(p);
        const markdown = optString(p, "markdown");
        const card = this.cardParam(p);
        if (markdown === undefined && card === undefined) {
          throw new RpcError(INVALID_PARAMS, "edit needs markdown or card");
        }
        await this.send(() => outbound.edit(ref, card !== undefined ? { card } : { markdown: markdown! }));
        return {};
      },

      delete: async (params, conn) => {
        this.requireActive(conn);
        const ref = this.refParam(paramsObject(params));
        await this.send(() => outbound.delete(ref));
        return {};
      },

      react: async (params, conn) => {
        this.requireActive(conn);
        const p = paramsObject(params);
        const ref = this.refParam(p);
        const emoji = reqString(p, "emoji");
        const remove = optBool(p, "remove") ?? false;
        await this.send(() => outbound.react(ref, emoji, remove));
        return {};
      },

      typing: async (params, conn) => {
        this.requireActive(conn);
        const target = reqString(paramsObject(params), "target");
        this.checkTarget(target);
        await this.send(() => outbound.typing(target));
        return {};
      },

      history: (params, conn) => {
        this.requireActive(conn);
        const p = paramsObject(params);
        const target = reqString(p, "target");
        this.checkTarget(target);
        const limit = reqInt(p, "limit", 1);
        const beforeText = optString(p, "before");
        let before: number | undefined;
        if (beforeText !== undefined) {
          if (!/^\d+$/.test(beforeText)) {
            throw new RpcError(INVALID_PARAMS, "before must be a seq number written as a string");
          }
          before = Number(beforeText);
        }
        const channel = this.channelOf(target);
        const rows = this.deps.log.history(
          channel === target ? { channelId: target } : { threadId: target },
          limit,
          before,
        );
        return { messages: rows.map((r) => this.wireHistory(r)) };
      },

      targets: (_params, conn) => {
        this.requireActive(conn);
        return {
          targets: repo.bindings().map((b) => ({
            channel: b.channelId,
            name: b.channelName,
            platform: b.platform,
            isDM: platforms.adapterFor(b.channelId)?.isDM?.(b.channelId) ?? false,
            threads: this.deps.log
              .threadsOf(b.channelId, TARGET_THREADS)
              .map((t) => ({ thread: t.thread, lastActiveAt: isoLocal(new Date(t.lastActiveAt)) })),
          })),
        };
      },

      origin: (params, conn) => {
        this.requireActive(conn);
        const p = paramsObject(params);
        const requesters = p.requesters;
        if (requesters !== null && requesters !== undefined && !(Array.isArray(requesters) && requesters.every((r) => typeof r === "string"))) {
          throw new RpcError(INVALID_PARAMS, "requesters must be an array of strings or null");
        }
        const thread = p.thread;
        if (thread !== null && thread !== undefined && typeof thread !== "string") {
          throw new RpcError(INVALID_PARAMS, "thread must be a string or null");
        }
        return { target: this.origin((requesters as string[] | null | undefined) ?? null, (thread as string | null | undefined) ?? null) };
      },
    };
  }

  /** DESIGN 6.8 `origin`: bound thread, else the first requester's notify_thread, else the first admin's. */
  private origin(requesters: string[] | null, thread: string | null): string | null {
    const { repo } = this.deps;
    if (thread !== null) {
      const channel = this.channelOf(thread);
      if (channel !== null && repo.binding(channel)) {
        return thread;
      }
    }
    const infos = repo.adminInfos();
    if (requesters !== null) {
      for (const r of requesters) {
        const t = infos.find((a) => a.identity === r)?.notifyThread;
        if (t) return t;
      }
      return null;
    }
    return infos.find((a) => a.notifyThread !== null)?.notifyThread ?? null;
  }

  private refParam(p: Params): string {
    const ref = reqString(p, "ref");
    let threadId: string;
    try {
      threadId = parseRef(ref).threadId;
    } catch (e) {
      throw new RpcError(INVALID_PARAMS, (e as Error).message);
    }
    this.checkTarget(threadId);
    return ref;
  }

  private cardParam(p: Params): CardElement | undefined {
    const card = p.card;
    if (card === undefined || card === null) {
      return undefined;
    }
    if (!isCardElement(card)) {
      throw new RpcError(INVALID_PARAMS, 'card must be a CardElement (type "card")');
    }
    return card;
  }

  private filesParam(p: Params): LoadedFile[] {
    const files = p.files;
    if (files === undefined || files === null) {
      return [];
    }
    if (!Array.isArray(files)) {
      throw new RpcError(INVALID_PARAMS, "files must be an array");
    }
    return files.map((f: unknown) => {
      const fp = paramsObject(f);
      const path = reqString(fp, "path");
      const name = optString(fp, "name");
      try {
        return this.deps.outbound.loadWorkspaceFile(path, name);
      } catch (e) {
        if (e instanceof OutboundError) {
          throw new RpcError(APP_ERROR, e.message);
        }
        throw e;
      }
    });
  }

  // ---- wire messages ----

  private withImageData(a: WireAttachment): WireAttachment {
    const { cfg } = this.deps;
    if (a.type !== "image" || !a.path || a.error !== undefined || a.size === undefined || a.size > cfg.imageMaxBytes) {
      return a;
    }
    // The agent can write the inbox: only the regular file at the saved path is read, never a symlink target.
    const expected = join(realpathSync(cfg.workspaceDir), a.path);
    let opened: OpenedFile;
    try {
      opened = openNoFollow(expected, constants.O_RDONLY);
    } catch (e) {
      const code = (e as NodeJS.ErrnoException).code;
      // The agent may have removed the inbox file before a replay.
      if (code === "ENOENT") {
        return { ...a, error: "文件已不存在" };
      }
      if (code === "ELOOP") {
        return { ...a, error: INBOX_FILE_REPLACED };
      }
      throw e;
    }
    try {
      if (opened.path !== expected || !opened.stats.isFile()) {
        return { ...a, error: INBOX_FILE_REPLACED };
      }
      if (opened.stats.size > cfg.imageMaxBytes) {
        return a;
      }
      return { ...a, data: readFileSync(opened.fd).toString("base64") };
    } finally {
      closeSync(opened.fd);
    }
  }

  private wireInbound(row: InboundRow, staleBefore: number): WireMessage {
    const { pending: _pending, ...payload } = row.payload;
    const stale = row.createdAt < staleBefore;
    return {
      ...payload,
      seq: [row.seq],
      stale,
      trigger: stale ? false : payload.trigger,
      attachments: payload.attachments.map((a) => this.withImageData(a)),
    };
  }

  private wireHistory(row: MessageRow): WireMessage {
    if (row.direction === "in") {
      const { pending: _pending, ...payload } = row.payload as InboundPayload;
      return { ...payload, seq: [row.seq], stale: false, direction: "in" };
    }
    const p = row.payload as OutboundPayload;
    const self = this.deps.platforms.selfNames()[p.platform] ?? this.deps.cfg.userName;
    return {
      seq: [row.seq],
      kind: "message",
      id: row.messageId,
      platform: p.platform,
      channel: p.channel,
      thread: row.threadId,
      sender: { id: `${p.platform}:bot`, name: self, role: "member", isBot: true },
      time: isoLocal(new Date(row.createdAt)),
      text: p.text,
      mentionsBot: false,
      edited: false,
      stale: false,
      attachments: p.attachments,
      trigger: false,
      direction: "out",
    };
  }
}
