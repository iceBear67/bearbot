import { createServer, type Server, type Socket } from "node:net";
import type { AgentState } from "./agent-state.ts";
import type { ChangeNotifier, DaemonClient } from "./commands.ts";
import type { GatewayConfig } from "./config.ts";
import type { PinoLogger } from "./log.ts";
import type { Outbound } from "./outbound.ts";
import type { Platforms } from "./platforms.ts";
import { CONSOLE_OPERATOR, type Repo } from "./repo.ts";
import {
  APP_ERROR,
  INVALID_PARAMS,
  JsonRpcConnection,
  RpcError,
  nullableString,
  optInt,
  paramsObject,
  reqString,
  type RequestHandler,
} from "./rpc.ts";
import { closeServer, listenUnix } from "./sockets.ts";
import { isoLocal } from "./time.ts";
import type { Tokens } from "./tokens.ts";
import { AGENT_WIRE_STATES, type AgentWireState } from "./types.ts";

/** A gateway request while bearbotd is not connected fails at once (DESIGN 3). */
export class DaemonUnavailableError extends RpcError {
  constructor() {
    super(APP_ERROR, "bearbotd 没有连接 gateway，请稍后再试");
    this.name = "DaemonUnavailableError";
  }
}

/** gateway-ctl.sock (contracts 4): one connection from bearbotd, requests in both directions. */
export class CtlServer implements DaemonClient {
  private readonly server: Server;
  private conn: JsonRpcConnection | null = null;

  constructor(
    private readonly deps: {
      cfg: GatewayConfig;
      repo: Repo;
      tokens: Tokens;
      outbound: Outbound;
      platforms: Platforms;
      agentState: AgentState;
      notifier: ChangeNotifier;
      /** Stops receiving from all platforms (`drain`). */
      drain: () => Promise<void>;
      logger: PinoLogger;
    },
  ) {
    this.server = createServer((socket) => this.accept(socket));
  }

  async listen(): Promise<void> {
    await listenUnix(this.server, this.deps.cfg.ctlSocket, 0o600);
    this.deps.logger.info({ socket: this.deps.cfg.ctlSocket }, "gateway-ctl.sock listening");
  }

  async close(): Promise<void> {
    this.conn?.destroy();
    await closeServer(this.server);
  }

  async request(method: string, params: object): Promise<unknown> {
    const conn = this.conn;
    if (!conn || conn.closed) {
      throw new DaemonUnavailableError();
    }
    return conn.request(method, params);
  }

  private accept(socket: Socket): void {
    const { logger } = this.deps;
    if (this.conn && !this.conn.closed) {
      logger.warn("second gateway-ctl.sock connection refused");
      socket.destroy();
      return;
    }
    const conn = new JsonRpcConnection(socket, {
      name: "gateway-ctl.sock",
      logger,
      idPrefix: "g",
      requests: this.handlers(),
    });
    this.conn = conn;
    logger.info("bearbotd connected to gateway-ctl.sock");
    conn.onClosed(() => {
      if (this.conn === conn) {
        this.conn = null;
      }
      logger.info("bearbotd disconnected from gateway-ctl.sock");
    });
  }

  private handlers(): Record<string, RequestHandler> {
    const { cfg, repo, tokens, outbound, platforms, agentState, notifier, drain, logger } = this.deps;
    return {
      hello: () => ({}),

      "platforms.status": () => ({ platforms: platforms.statuses() }),

      "admin.createToken": (params) => {
        const p = paramsObject(params);
        const ttlMs = optInt(p, "ttlMs", 1) ?? cfg.tokenTtlMs;
        const { token, expiresAt } = tokens.create(ttlMs);
        logger.info({ expiresAt: expiresAt.toISOString() }, "admin token created");
        return { token, expiresAt: isoLocal(expiresAt) };
      },

      "admin.list": () => ({ admins: repo.adminInfos() }),

      "admin.revoke": (params) => {
        const identity = reqString(paramsObject(params), "identity");
        if (identity === CONSOLE_OPERATOR && repo.isBuiltinAdmin(identity)) {
          throw new RpcError(APP_ERROR, "内置管理员不能撤销");
        }
        if (!repo.removeAdmin(identity)) {
          throw new RpcError(APP_ERROR, `${identity} 不是管理员`);
        }
        notifier.adminsChanged();
        return {};
      },

      "agent.state": (params) => {
        const p = paramsObject(params);
        const state = reqString(p, "state");
        if (!(AGENT_WIRE_STATES as readonly string[]).includes(state)) {
          throw new RpcError(INVALID_PARAMS, `unknown agent state: ${state}`);
        }
        agentState.update(state as AgentWireState, nullableString(p, "reason"));
        return {};
      },

      notify: async (params) => {
        const text = reqString(paramsObject(params), "text");
        await Promise.all(
          repo.notifyThreads().map(async (thread) => {
            try {
              await outbound.post(thread, { text });
            } catch (e) {
              // A platform send failure is logged; the other admins still get the notice.
              logger.error({ thread, err: e }, "notify to an admin thread failed");
            }
          }),
        );
        return {};
      },

      drain: async () => {
        await drain();
        logger.info("drained: no more events are received");
        return {};
      },
    };
  }
}
