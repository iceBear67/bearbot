import { TelegramAdapter, type TelegramAdapterConfig } from "@chat-adapter/telegram";
import { QQAdapter, type QQSocketModeAdapterConfig } from "@amatsuka/chat-adapter-qq";
import { AuthenticationError } from "@chat-adapter/shared";
import type { Adapter, ChatInstance, Logger as ChatLogger } from "chat";
import type { GatewayConfig } from "./config.ts";
import { ConsoleAdapter } from "./console-adapter.ts";
import { observedLogger, type LogObserver, type PinoLogger } from "./log.ts";
import { StatusRecorder } from "./platform-status.ts";
import { platformOf } from "./repo.ts";
import type { PlatformStatus } from "./types.ts";

const FIRST_RETRY_MS = 1000;
const MAX_RETRY_MS = 60_000;

/**
 * Runs the first connection of a platform in the background so that `bot.initialize()` never rejects
 * (contracts 9.4): a failed attempt is recorded and retried with backoff 1 s to 60 s.
 */
export class ConnectLoop {
  private stopped = false;
  private task: Promise<void> | null = null;
  private wake: (() => void) | null = null;

  constructor(
    private readonly status: StatusRecorder,
    private readonly logger: PinoLogger,
  ) {}

  start(attempt: () => Promise<void>): void {
    if (this.task) {
      throw new Error(`${this.status.name}: connection loop already started`);
    }
    this.task = this.run(attempt);
  }

  private async run(attempt: () => Promise<void>): Promise<void> {
    let delay = FIRST_RETRY_MS;
    while (!this.stopped) {
      this.status.connecting();
      try {
        await attempt();
        this.status.connected();
        this.logger.info({ platform: this.status.name }, "platform connected");
        return;
      } catch (e) {
        this.status.failed(e);
        this.logger.warn({ platform: this.status.name, err: e, retryInMs: delay }, "platform connection failed");
      }
      if (this.stopped) {
        return;
      }
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, delay);
        this.wake = () => {
          clearTimeout(timer);
          resolve();
        };
      });
      this.wake = null;
      delay = Math.min(delay * 2, MAX_RETRY_MS);
    }
  }

  /** Ends the loop; when the connection was made, `stopReceiving` closes it. */
  async stop(stopReceiving: () => Promise<void>): Promise<void> {
    this.stopped = true;
    this.wake?.();
    await this.task;
    await stopReceiving();
  }
}

function isAbort(e: unknown): boolean {
  return e instanceof Error && e.name === "AbortError";
}

export class GatewayTelegramAdapter extends TelegramAdapter {
  readonly status: StatusRecorder;
  private readonly loop: ConnectLoop;

  constructor(config: TelegramAdapterConfig, status: StatusRecorder, logger: PinoLogger) {
    super(config);
    this.status = status;
    this.loop = new ConnectLoop(status, logger);
  }

  override async initialize(chat: ChatInstance): Promise<void> {
    this.loop.start(() => super.initialize(chat));
  }

  protected override async telegramFetch<TResult>(
    method: string,
    payload?: Record<string, unknown> | FormData,
    request?: { signal?: AbortSignal },
  ): Promise<TResult> {
    if (method !== "getUpdates") {
      return super.telegramFetch<TResult>(method, payload, request);
    }
    let result: TResult;
    try {
      result = await super.telegramFetch<TResult>(method, payload, request);
    } catch (e) {
      if (!isAbort(e)) {
        if (e instanceof AuthenticationError) {
          this.status.failed(e);
        } else {
          this.status.disconnected(e);
        }
      }
      throw e;
    }
    this.status.connected();
    return result;
  }

  /** drain: stop polling (contracts 4.1). */
  async stopReceiving(): Promise<void> {
    await this.loop.stop(() => this.stopPolling());
  }
}

export class GatewayQQAdapter extends QQAdapter {
  readonly status: StatusRecorder;
  private readonly loop: ConnectLoop;

  constructor(config: QQSocketModeAdapterConfig, status: StatusRecorder, logger: PinoLogger) {
    super(config);
    this.status = status;
    this.loop = new ConnectLoop(status, logger);
  }

  override async initialize(chat: ChatInstance): Promise<void> {
    this.loop.start(() => super.initialize(chat));
  }

  /** drain: stop socket mode (contracts 4.1). */
  async stopReceiving(): Promise<void> {
    await this.loop.stop(() => this.stopSocketMode());
  }
}

/** Connection status of the QQ adapter from its log lines (the adapter has no status API). */
function qqStatusObserver(status: StatusRecorder, stopping: () => boolean): LogObserver {
  const detail = (args: unknown[]): string => {
    const [first] = args;
    if (first instanceof Error) return first.message;
    if (first && typeof first === "object") return JSON.stringify(first);
    return first === undefined ? "" : String(first);
  };
  return (_level, message, args) => {
    if (stopping()) {
      return;
    }
    switch (message) {
      case "QQ gateway connected":
        status.connected();
        break;
      case "QQ gateway closed":
        status.disconnected();
        break;
      case "QQ gateway socket error":
      case "QQ gateway reconnect failed":
      case "QQ gateway message handling failed":
      case "QQ gateway heartbeat ACK timed out; reconnecting":
      case "QQ gateway session invalidated":
        status.recordError(`${message} ${detail(args)}`.trim());
        break;
      case "QQ gateway closed with a terminal configuration/authentication code":
        status.failed(`${message} ${detail(args)}`.trim());
        break;
    }
  };
}

/** The adapters registered in the Chat object: only enabled platforms (DESIGN 6.1). */
export class Platforms {
  readonly adapters: Record<string, Adapter> = {};
  readonly telegram: GatewayTelegramAdapter | null = null;
  readonly qq: GatewayQQAdapter | null = null;
  readonly console: ConsoleAdapter | null = null;
  private stopping = false;
  private stopTask: Promise<void> | null = null;

  constructor(cfg: GatewayConfig, logger: PinoLogger, chatLogger: ChatLogger) {
    if (cfg.platforms.telegram) {
      const status = new StatusRecorder("telegram", "connecting");
      // Created without userName so that getMe sets the real bot name (contracts 9.4).
      this.telegram = new GatewayTelegramAdapter(
        {
          botToken: cfg.platforms.telegram.botToken,
          mode: "polling",
          mentionOnReply: true,
          logger: chatLogger.child("telegram"),
          longPolling: { allowedUpdates: ["message", "edited_message", "callback_query", "message_reaction"] },
        },
        status,
        logger,
      );
      this.adapters.telegram = this.telegram;
    }
    if (cfg.platforms.qq) {
      const status = new StatusRecorder("qq", "connecting");
      this.qq = new GatewayQQAdapter(
        {
          appId: cfg.platforms.qq.appId,
          clientSecret: cfg.platforms.qq.clientSecret,
          mode: "socket",
          sandbox: cfg.platforms.qq.sandbox,
          userName: cfg.userName,
          logger: observedLogger(chatLogger.child("qq"), qqStatusObserver(status, () => this.stopping)),
        },
        status,
        logger,
      );
      this.adapters.qq = this.qq;
    }
    if (cfg.console) {
      this.console = new ConsoleAdapter({
        inputFd: cfg.console.inputFd,
        outputFd: cfg.console.outputFd,
        filesDir: cfg.consoleFilesDir,
        workspaceDir: cfg.workspaceDir,
        userName: cfg.userName,
        logger: logger.child({ scope: "console" }),
      });
      this.adapters.console = this.console;
    }
  }

  names(): Set<string> {
    return new Set(Object.keys(this.adapters));
  }

  /** The adapter of a thread id, channel id or message ref; undefined for platforms that are not enabled. */
  adapterFor(id: string): Adapter | undefined {
    return this.adapters[platformOf(id)];
  }

  statuses(): PlatformStatus[] {
    const list: PlatformStatus[] = [];
    if (this.telegram) list.push(this.telegram.status.snapshot());
    if (this.qq) list.push(this.qq.status.snapshot());
    if (this.console) list.push(this.console.status.snapshot());
    return list;
  }

  /** Bot user name per platform for `hello.self`. */
  selfNames(): Record<string, string> {
    const self: Record<string, string> = {};
    for (const [name, adapter] of Object.entries(this.adapters)) {
      self[name] = adapter.userName;
    }
    return self;
  }

  /** Stops receiving from every platform; outbound sending keeps working. */
  stopReceiving(): Promise<void> {
    this.stopTask ??= (async () => {
      this.stopping = true;
      await Promise.all([this.telegram?.stopReceiving(), this.qq?.stopReceiving()]);
      this.console?.stopInput();
    })();
    return this.stopTask;
  }
}
