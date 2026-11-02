import { SlackAdapter, type SlackAdapterConfig } from "@chat-adapter/slack";
import { TelegramAdapter, type TelegramAdapterConfig } from "@chat-adapter/telegram";
import { QQAdapter, type QQSocketModeAdapterConfig } from "@amatsuka/chat-adapter-qq";
import { AuthenticationError, ValidationError } from "@chat-adapter/shared";
import { SocketModeClient } from "@slack/socket-mode";
import type { Adapter, ChatInstance, Logger as ChatLogger } from "chat";
import type { GatewayConfig } from "./config.ts";
import { ConsoleAdapter } from "./console-adapter.ts";
import { observedLogger, type LogObserver, type PinoLogger } from "./log.ts";
import { StatusRecorder } from "./platform-status.ts";
import { platformOf } from "./repo.ts";
import type { PlatformStatus } from "./types.ts";

const FIRST_RETRY_MS = 1000;
const MAX_RETRY_MS = 60_000;
/** Timeout of one Slack Web API request attempt (DESIGN 6.2.3). */
const SLACK_REQUEST_TIMEOUT_MS = 30_000;
/**
 * Retries of one Slack Web API call. The library default retries for about 30 minutes, which would hold every send
 * that waits for it (DESIGN 6.2.3); a rate-limited call already waits for the Retry-After header before it throws,
 * so two more attempts cover the usual case and a persistent failure turns into a visible error.
 */
const SLACK_RETRY_CONFIG = { retries: 2, minTimeout: 500, maxTimeout: 2000, randomize: true };

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
  /** `[platforms.qq] markdown`: text goes out as native markdown messages, which need a platform permission. */
  readonly markdown: boolean;
  private readonly loop: ConnectLoop;

  constructor(config: QQSocketModeAdapterConfig, markdown: boolean, status: StatusRecorder, logger: PinoLogger) {
    super(config);
    this.markdown = markdown;
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

/**
 * The official Slack adapter in socket mode (DESIGN 6.2.3). `initialize()` resolves only after Slack sent the
 * `hello` frame over the socket and rejects when the first connection cannot be made, so the first connection goes
 * into the same retry loop as Telegram and QQ. After that `@slack/socket-mode` reconnects by itself.
 */
export class GatewaySlackAdapter extends SlackAdapter {
  readonly status: StatusRecorder;
  private readonly loop: ConnectLoop;
  private readonly channelNames = new Map<string, string>();
  /** The client this subclass built in `startSocketMode`; the library keeps its own in a private field. */
  private socketClientForStop: SocketModeClient | null = null;

  constructor(config: SlackAdapterConfig, status: StatusRecorder, logger: PinoLogger) {
    super(config);
    this.status = status;
    this.loop = new ConnectLoop(status, logger);
  }

  override async initialize(chat: ChatInstance): Promise<void> {
    this.loop.start(async () => {
      // The adapter's own bot user id lookup logs a failure and connects anyway, and without the id it cannot tell
      // the bot's own messages apart: they would be recorded and, in a channel whose trigger is `all`, start a
      // reply loop. The lookup therefore happens here, where a failure is a failed connection and is retried.
      if (!this.botUserId) {
        const auth = await this.webClient.auth.test({});
        if (typeof auth.user_id !== "string") {
          throw new Error(`Slack auth.test 没有返回 user_id：${JSON.stringify(auth)}`);
        }
        this._botUserId = auth.user_id;
        this._botId = typeof auth.bot_id === "string" ? auth.bot_id : null;
      }
      await super.initialize(chat);
    });
  }

  /** The channel name already looked up, without a Web API call (DESIGN 6.2.3). */
  cachedChannelName(channelId: string): string | null {
    return this.channelNames.get(channelId) ?? null;
  }

  /**
   * Looks up the name of a channel ("#general", the other user's name in a DM) with `conversations.info` and caches
   * it; Slack message events carry no name (DESIGN 6.2.3). Ingestion never waits for this: the call is started in
   * the background and the name is used from the next message on. A failure is only recorded in the platform status.
   */
  async refreshChannelName(channelId: string): Promise<void> {
    if (this.channelNames.has(channelId)) {
      return;
    }
    const { channel } = this.decodeThreadId(channelId);
    try {
      const result = await this.webClient.conversations.info({ channel });
      const info = result.channel as { name?: string; is_im?: boolean; user?: string } | undefined;
      let name: string | null = null;
      if (info?.is_im === true && info.user !== undefined) {
        const user = await this.getUser(info.user);
        name = user ? user.fullName : null;
      } else if (info?.name !== undefined) {
        name = `#${info.name}`;
      }
      if (name !== null) {
        this.channelNames.set(channelId, name);
      }
    } catch (e) {
      this.status.recordError(e);
    }
  }

  /**
   * `startSocketMode()` of the library builds its own `SocketModeClient`, whose Web API client retries
   * `apps.connections.open` up to 100 times with growing delays; a wrong app token would keep `start()` running for
   * hours and `disconnect()` never stops it. The client is built here with the same settings and a retry count the
   * ConnectLoop bounds, and connected the same way the library does.
   */
  protected override async startSocketMode(): Promise<void> {
    if (!this.appToken) {
      throw new ValidationError("slack", "appToken is required for socket mode");
    }
    const client = new SocketModeClient({
      appToken: this.appToken,
      clientOptions: {
        retryConfig: SLACK_RETRY_CONFIG,
        timeout: SLACK_REQUEST_TIMEOUT_MS,
        ...(this.slackApiUrl !== undefined ? { slackApiUrl: this.slackApiUrl } : {}),
      },
    });
    client.on("slack_event", ({ ack, body, type, retry_num, retry_reason }) => {
      void this.routeSocketEvent(body, type, ack, undefined, retry_num).catch((error: unknown) => {
        this.logger.error("Error processing socket mode event", { type, retry_num, retry_reason, error });
      });
    });
    this.socketClientForStop = client;
    await client.start();
    this.logger.info("Slack socket mode connected");
  }

  /** drain: stop socket mode (contracts 4.1). */
  async stopReceiving(): Promise<void> {
    await this.loop.stop(async () => {
      await this.socketClientForStop?.disconnect();
      this.socketClientForStop = null;
      await this.disconnect();
    });
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
  readonly slack: GatewaySlackAdapter | null = null;
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
        cfg.platforms.qq.markdown,
        status,
        logger,
      );
      this.adapters.qq = this.qq;
    }
    if (cfg.platforms.slack) {
      const status = new StatusRecorder("slack", "connecting");
      this.slack = new GatewaySlackAdapter(
        {
          botToken: cfg.platforms.slack.botToken,
          appToken: cfg.platforms.slack.appToken,
          mode: "socket",
          userName: cfg.userName,
          logger: chatLogger.child("slack"),
          webClientOptions: { retryConfig: SLACK_RETRY_CONFIG, timeout: SLACK_REQUEST_TIMEOUT_MS },
        },
        status,
        logger,
      );
      this.adapters.slack = this.slack;
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
    if (this.slack) list.push(this.slack.status.snapshot());
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
      await Promise.all([this.telegram?.stopReceiving(), this.qq?.stopReceiving(), this.slack?.stopReceiving()]);
      this.console?.stopInput();
    })();
    return this.stopTask;
  }
}
