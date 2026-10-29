import { randomUUID } from "node:crypto";
import type { AssistantMessage } from "@earendil-works/pi-ai";
import type {
  AgentSettledEvent,
  BeforeAgentStartEvent,
  ExtensionAPI,
  ExtensionContext,
  InputEvent,
  InputEventResult,
} from "@earendil-works/pi-coding-agent";
import type { InboundMessage, OutboundReply, PromptRequest, Role, Sender, TurnInfo } from "../../../sdk/index.ts";
import type { AgentClient } from "./agent-client.ts";
import { classifierMessages, type ClassifierTurn, rulesFromTurnStart } from "./classifier.ts";
import type { Diagnostics } from "./diagnostics.ts";
import type { GatewayClient, TargetsResult, TurnNotification, WireBinding } from "./gateway-client.ts";
import type { AckTracker, OutboundPipeline, OutboundRun, Registry, RouteTarget } from "./pipeline.ts";
import {
  assistantText,
  collectImages,
  localIso,
  refDateOf,
  renderMessageTurn,
  renderPromptTurn,
  renderSystemTurn,
  userContent,
} from "./render.ts";
import { QUEUED_SUMMARY_TIMEOUT_MS, summarizeQueued } from "./queued.ts";
import { rollbackAckAllowed, type RollbackState } from "./rollback.ts";

// Dispatch queue and turn lifecycle (DESIGN 7.2).

export type SystemTurnKind = "applied" | "incident" | "rollback";

export interface SystemTurnSpec {
  kind: SystemTurnKind;
  origin: string | null;
  attrs: [string, string | null][];
  /** Evaluated when the turn is dispatched, so the lists of tools and middleware are current. */
  body: () => string[];
}

type QueueEntry =
  | { kind: "message"; msg: InboundMessage }
  | { kind: "prompt"; request: PromptRequest; extension: string; dispatched: () => void }
  | { kind: "system"; spec: SystemTurnSpec };

type Batch =
  | {
    kind: "message";
    thread: string;
    messages: InboundMessage[];
    observed: InboundMessage[];
    /** Queued trigger messages of other senders in the thread; they stay queued and are shown as observed. */
    queued: InboundMessage[];
    entries: QueueEntry[];
  }
  | { kind: "prompt"; entry: Extract<QueueEntry, { kind: "prompt" }> }
  | { kind: "system"; entry: Extract<QueueEntry, { kind: "system" }> };

/** One dispatch queue for every bound channel plus the observed buffers per thread. */
export class TurnQueue implements RouteTarget {
  private entries: QueueEntry[] = [];
  private readonly observedByThread = new Map<string, InboundMessage[]>();

  constructor(
    private readonly observeLimit: number,
    private readonly ack: AckTracker,
  ) {}

  /** Routing step of 7.2: not triggering (or stale) messages go to the observed buffer, the others are queued. */
  route(msg: InboundMessage): void {
    if (!msg.trigger || msg.stale) this.observe(msg);
    else this.entries.push({ kind: "message", msg });
  }

  observe(msg: InboundMessage): void {
    if (this.observeLimit > 0) {
      const list = this.observedByThread.get(msg.thread) ?? [];
      list.push(msg);
      while (list.length > this.observeLimit) list.shift();
      this.observedByThread.set(msg.thread, list);
    }
    this.ack.complete(msg.seq);
  }

  push(entry: QueueEntry): void {
    this.entries.push(entry);
  }

  /** Puts a batch that could not be dispatched back at the front. */
  restore(batch: Batch): void {
    if (batch.kind === "message") {
      this.entries.unshift(...batch.entries);
      const current = this.observedByThread.get(batch.thread) ?? [];
      this.observedByThread.set(batch.thread, [...batch.observed, ...current].slice(-Math.max(this.observeLimit, 0)));
    } else {
      this.entries.unshift(batch.entry);
    }
  }

  /**
   * The next turn: the earliest entry decides; for a chat message every queued message of its thread from the same
   * sender with the same role joins, so a turn has one sender. The role is compared as well: a sender whose admin
   * status changed between two messages has them in separate turns, so member text never runs in an admin turn.
   */
  takeNext(): Batch | null {
    const first = this.entries[0];
    if (!first) return null;
    if (first.kind === "prompt") {
      this.entries.shift();
      return { kind: "prompt", entry: first };
    }
    if (first.kind === "system") {
      this.entries.shift();
      return { kind: "system", entry: first };
    }
    const thread = first.msg.thread;
    const sender = first.msg.sender;
    const taken: QueueEntry[] = [];
    const queued: InboundMessage[] = [];
    this.entries = this.entries.filter((entry) => {
      if (entry.kind !== "message" || entry.msg.thread !== thread) return true;
      if (entry.msg.sender.id !== sender.id) {
        queued.push(entry.msg);
        return true;
      }
      // The same person with the other role: a turn of its own later, not shown as someone else's queued message.
      if (entry.msg.sender.role !== sender.role) return true;
      taken.push(entry);
      return false;
    });
    const messages = taken.map((entry) => (entry as Extract<QueueEntry, { kind: "message" }>).msg);
    return { kind: "message", thread, messages, observed: this.takeObserved(thread), queued, entries: taken };
  }

  takeObserved(thread: string): InboundMessage[] {
    const list = this.observedByThread.get(thread) ?? [];
    this.observedByThread.delete(thread);
    return list;
  }

  observedCount(thread: string): number {
    return this.observedByThread.get(thread)?.length ?? 0;
  }

  get size(): number {
    return this.entries.length;
  }

  summary(): string[] {
    return this.entries.map((entry) => {
      if (entry.kind === "message") return `消息 seq ${entry.msg.seq.join(",")}（${entry.msg.thread}，${entry.msg.sender.id}）`;
      if (entry.kind === "prompt") return `扩展 ${entry.extension} 的 prompt（${entry.request.role}，目标 ${entry.request.origin ?? "无"}）`;
      return `系统回合 ${entry.spec.kind}（目标 ${entry.spec.origin ?? "无"}）`;
    });
  }
}

export function shouldSendReply(text: string): boolean {
  const trimmed = text.trim();
  return trimmed !== "" && trimmed !== "NO_REPLY";
}

/** Finds the binding of a channel id or of a thread id inside a bound channel. */
export function bindingFor(bindings: readonly WireBinding[], target: string): WireBinding | undefined {
  return bindings.find((binding) => binding.channel === target)
    ?? bindings.find((binding) => target.startsWith(`${binding.channel}:`));
}

/**
 * Whether a routed message is steered into the current turn (7.2): a trigger message of the turn's own sender, with
 * the turn's role, in the turn's thread, while the turn is dispatched or running, in a channel bound with
 * `interrupt = steer` or marked by `!steer`. prompt() and system turns have no sender, so nothing is steered into them.
 */
export function canSteer(
  turn: Pick<ActiveTurn, "kind" | "phase" | "origin" | "role" | "senders">,
  msg: InboundMessage,
  binding: WireBinding | undefined,
): boolean {
  return turn.kind === "message"
    && (turn.phase === "dispatched" || turn.phase === "running")
    && msg.trigger
    && !msg.stale
    && turn.origin === msg.thread
    && msg.sender.id === turn.senders[0].id
    && msg.sender.role === turn.role
    && (binding?.interrupt === "steer" || msg.steer === true);
}

export type TurnPhase = "preparing" | "dispatched" | "running" | "ending";

export interface ActiveTurn {
  id: string;
  kind: "message" | "prompt" | "system";
  systemKind: SystemTurnKind | null;
  extension: string | null;
  origin: string | null;
  role: Role;
  /** Exactly one sender for a message turn, none for prompt() and system turns. */
  senders: Sender[];
  messages: InboundMessage[];
  seqs: number[];
  /** Texts given to pi.sendUserMessage for this turn (the turn text and the steered messages). */
  texts: string[];
  viaPrompt: boolean;
  startedAt: string;
  phase: TurnPhase;
  /** onTurnStart listeners ran; a turn that failed before dispatch gets no onTurnEnd either. */
  listenersStarted: boolean;
  replyMode: "final" | "each";
  lastAssistant: AssistantMessage | null;
  classifier: ClassifierTurn | null;
  eachChain: Promise<void>;
  typingTimer: NodeJS.Timeout | null;
  typingFailed: boolean;
  watchdog: NodeJS.Timeout | null;
  idleTicks: number;
}

export interface TurnResult {
  text: string;
  aborted: boolean;
  error?: string;
}

export type TurnStartListener = (turn: TurnInfo) => void;
export type TurnEndListener = (turn: TurnInfo, result: TurnResult) => void;

export interface DispatcherDeps {
  pi: () => ExtensionAPI | null;
  ctx: () => ExtensionContext | null;
  canDispatch: () => boolean;
  queue: TurnQueue;
  ack: AckTracker;
  gateway: GatewayClient;
  agent: AgentClient;
  diagnostics: Diagnostics;
  outbound: OutboundPipeline;
  sendReply: (reply: OutboundReply) => Promise<{ ref: string }>;
  bindings: () => readonly WireBinding[];
  rollback: RollbackState;
  turnStartListeners: Registry<TurnStartListener>;
  turnEndListeners: Registry<TurnEndListener>;
  errorNotice: string;
  rulesMaxBytes: number;
  /** Queued messages of other senders above this count are compressed by the model (`[bridge] queued_summary_after`). */
  queuedSummaryAfter: number;
  /** System prompt of the summary request (prompt/queued-summary.md). */
  queuedSummaryPrompt: string;
}

const TYPING_INTERVAL_MS = 5000;
const IDLE_CHECK_MS = 1000;
/** A dispatched prompt that pi neither starts nor rejects (no error event arrives) ends after this long idle time. */
const DISPATCH_IDLE_LIMIT = 60;
const BUSY_RETRY_MS = 500;

function errorDetail(error: unknown): string {
  return error instanceof Error ? (error.stack ?? error.message) : String(error);
}

export class Dispatcher implements RouteTarget {
  private current: ActiveTurn | null = null;
  private foreignRun = false;
  private starting = false;
  private retryTimer: NodeJS.Timeout | null = null;
  private readonly expectedInputs: { text: string; turnId: string }[] = [];
  /** Texts of turns the watchdog ended while pi may still hold them; pi can still start or reject them later. */
  private readonly abandoned: string[] = [];

  constructor(private readonly deps: DispatcherDeps) {}

  get turn(): ActiveTurn | null {
    return this.current;
  }

  get busyWithForeignRun(): boolean {
    return this.foreignRun;
  }

  turnInfo(turn: ActiveTurn): TurnInfo {
    return { origin: turn.origin ?? "", role: turn.role, senders: turn.senders.map((sender) => ({ ...sender })), messages: turn.messages };
  }

  /** Routing target of the inbound pipeline: steer into the current turn when 7.2 allows it, otherwise queue. */
  route(msg: InboundMessage): void {
    const turn = this.current;
    if (turn && canSteer(turn, msg, bindingFor(this.deps.bindings(), msg.channel.id))) {
      const pi = this.deps.pi();
      if (pi) {
        this.steer(pi, turn, msg);
        return;
      }
    }
    this.deps.queue.route(msg);
    this.kick();
  }

  prompt(request: PromptRequest, extension: string): Promise<void> {
    if (typeof request?.text !== "string" || request.text.trim() === "") throw new Error("prompt 需要非空的 text");
    if (request.role !== "admin" && request.role !== "member") throw new Error(`prompt 的 role 只能是 admin 或 member：${String(request.role)}`);
    if (request.origin !== undefined && (typeof request.origin !== "string" || request.origin === "")) {
      throw new Error("prompt 的 origin 必须是非空的字符串");
    }
    for (const image of request.images ?? []) {
      if (typeof image?.data !== "string" || typeof image?.mimeType !== "string") throw new Error("prompt 的 images 需要 data 与 mimeType");
    }
    return new Promise<void>((resolve) => {
      this.deps.queue.push({ kind: "prompt", request, extension, dispatched: resolve });
      this.kick();
    });
  }

  system(spec: SystemTurnSpec): void {
    this.deps.queue.push({ kind: "system", spec });
    this.kick();
  }

  /** Tries to start the next turn. */
  kick(): void {
    if (this.retryTimer) {
      clearTimeout(this.retryTimer);
      this.retryTimer = null;
    }
    if (this.starting || this.current || this.foreignRun) return;
    if (this.deps.queue.size === 0 || !this.deps.canDispatch()) return;
    const pi = this.deps.pi();
    const ctx = this.deps.ctx();
    if (!pi || !ctx) return;
    if (!ctx.isIdle()) {
      // pi is busy (for example a manual compaction); look again shortly.
      this.retryTimer = setTimeout(() => this.kick(), BUSY_RETRY_MS);
      this.retryTimer.unref();
      return;
    }
    const batch = this.deps.queue.takeNext();
    if (!batch) return;
    this.starting = true;
    this.start(batch).then(
      () => {
        this.starting = false;
        this.kick();
      },
      (error: unknown) => {
        this.starting = false;
        throw error;
      },
    );
  }

  private newTurn(batch: Batch): ActiveTurn {
    const base = {
      id: randomUUID(),
      startedAt: localIso(new Date()),
      texts: [] as string[],
      phase: "preparing" as TurnPhase,
      listenersStarted: false,
      lastAssistant: null,
      classifier: null,
      eachChain: Promise.resolve(),
      typingTimer: null,
      typingFailed: false,
      watchdog: null,
      idleTicks: 0,
    };
    if (batch.kind === "message") {
      // takeNext() gives the messages of one sender.
      const sender = batch.messages[0].sender;
      const binding = bindingFor(this.deps.bindings(), batch.messages[0].channel.id);
      return {
        ...base,
        kind: "message",
        systemKind: null,
        extension: null,
        origin: batch.thread,
        role: sender.role,
        senders: [{ ...sender }],
        messages: [...batch.messages],
        seqs: batch.messages.flatMap((message) => message.seq),
        viaPrompt: false,
        replyMode: binding?.replyMode ?? "final",
      };
    }
    if (batch.kind === "prompt") {
      const origin = batch.entry.request.origin ?? null;
      return {
        ...base,
        kind: "prompt",
        systemKind: null,
        extension: batch.entry.extension,
        origin,
        role: batch.entry.request.role,
        senders: [],
        messages: [],
        seqs: [],
        viaPrompt: true,
        replyMode: origin === null ? "final" : (bindingFor(this.deps.bindings(), origin)?.replyMode ?? "final"),
      };
    }
    const origin = batch.entry.spec.origin;
    return {
      ...base,
      kind: "system",
      systemKind: batch.entry.spec.kind,
      extension: null,
      origin,
      role: "system",
      senders: [],
      messages: [],
      seqs: [],
      viaPrompt: false,
      replyMode: origin === null ? "final" : (bindingFor(this.deps.bindings(), origin)?.replyMode ?? "final"),
    };
  }

  /** `<channel>` of a prompt() turn: the bound channel of the origin, with its type from the gateway `targets` answer. */
  private async promptChannel(origin: string | null): Promise<Pick<InboundMessage, "channel" | "platform"> | null> {
    if (origin === null) return null;
    const binding = bindingFor(this.deps.bindings(), origin);
    if (!binding) return null;
    const result = await this.deps.gateway.request<TargetsResult>("targets", {});
    const target = result.targets.find((entry) => entry.channel === binding.channel);
    if (!target) return null;
    return {
      channel: { id: binding.channel, ...(binding.name === null ? {} : { name: binding.name }), isDM: target.isDM },
      platform: binding.platform,
    };
  }

  private renderBatch(
    batch: Batch,
    turn: ActiveTurn,
    promptChannel: Pick<InboundMessage, "channel" | "platform"> | null,
    queuedSummary: string | null,
  ): { text: string; images: { type: "image"; data: string; mimeType: string }[] } {
    if (batch.kind === "message") {
      return {
        text: renderMessageTurn({
          role: turn.role,
          origin: batch.thread,
          messages: batch.messages,
          observed: batch.observed,
          queued: batch.queued,
          ...(queuedSummary === null ? {} : { queuedSummary }),
        }),
        images: collectImages(batch.messages),
      };
    }
    if (batch.kind === "prompt") {
      return {
        text: renderPromptTurn({
          role: batch.entry.request.role,
          origin: turn.origin,
          extension: batch.entry.extension,
          text: batch.entry.request.text,
          channel: promptChannel,
        }),
        images: (batch.entry.request.images ?? []).map((image) => ({ type: "image" as const, data: image.data, mimeType: image.mimeType })),
      };
    }
    return {
      text: renderSystemTurn({ kind: batch.entry.spec.kind, origin: turn.origin, attrs: batch.entry.spec.attrs, body: batch.entry.spec.body() }),
      images: [],
    };
  }

  private async start(batch: Batch): Promise<void> {
    const turn = this.newTurn(batch);
    this.current = turn;
    let queuedSummary: string | null = null;
    if (batch.kind === "message" && batch.queued.length > this.deps.queuedSummaryAfter) {
      const ctx = this.deps.ctx();
      const summary = ctx
        ? await summarizeQueued(ctx, this.deps.queuedSummaryPrompt, batch.queued, refDateOf(batch.messages, new Date()), QUEUED_SUMMARY_TIMEOUT_MS)
        : { kind: "failed" as const, error: "pi 的运行时当前不可用" };
      if (summary.kind === "failed") {
        // The turn ends with an error like a turn pi rejected; its messages are complete and not dispatched again.
        this.deps.ack.complete(turn.seqs);
        await this.finish(turn, { aborted: false, error: `排队中的 ${batch.queued.length} 条消息压缩失败：${summary.error}` });
        return;
      }
      queuedSummary = summary.text;
    }
    if (turn.role === "member") {
      // A failed start rejects start(), and kick() lets the rejection end the process: the batch is not acked, so
      // the gateway sends it again, and the daemon compares a record it opened when the process exits.
      const start = await this.deps.agent.classifierTurnStart(turn.id, turn.senders.map((sender) => sender.id));
      turn.classifier = {
        id: turn.id,
        role: turn.role,
        rules: rulesFromTurnStart(start, this.deps.rulesMaxBytes),
        rulesWarned: false,
        cache: new Map(),
        messages: () => (turn.kind === "prompt" && batch.kind === "prompt"
          ? [{ from: `扩展 ${batch.entry.extension}`, role: batch.entry.request.role, text: batch.entry.request.text }, ...classifierMessages(turn.messages)]
          : classifierMessages(turn.messages)),
      };
    }
    const promptChannel = batch.kind === "prompt" ? await this.promptChannel(turn.origin) : null;
    const pi = this.deps.pi();
    if (!pi) {
      // The runtime is being replaced; the batch waits for the next session_start.
      if (turn.classifier) await this.endClassifierTurn(turn);
      this.current = null;
      this.deps.queue.restore(batch);
      return;
    }
    const { text, images } = this.renderBatch(batch, turn, promptChannel, queuedSummary);
    turn.phase = "dispatched";
    turn.texts.push(text);
    this.expectInput(text, turn.id);
    pi.sendUserMessage(images.length > 0 ? userContent(text, images) : text);
    this.deps.ack.complete(turn.seqs);
    this.startTyping(turn);
    this.startWatchdog(turn);
    this.notifyGateway(turn);
    const info = this.turnInfo(turn);
    turn.listenersStarted = true;
    for (const entry of [...this.deps.turnStartListeners.list()]) {
      try {
        entry.fn(info);
      } catch (error) {
        this.deps.diagnostics.add({
          source: `listener:${entry.owner.name}`,
          level: "error",
          message: `扩展 ${entry.owner.name} 的 onTurnStart 监听函数抛出异常`,
          detail: errorDetail(error),
        });
      }
    }
    if (batch.kind === "prompt") batch.entry.dispatched();
  }

  private steer(pi: ExtensionAPI, turn: ActiveTurn, msg: InboundMessage): void {
    const observed = this.deps.queue.takeObserved(msg.thread);
    const text = renderMessageTurn({ role: turn.role, origin: msg.thread, messages: [msg], observed, queued: [], kind: "steer" });
    const images = collectImages([msg]);
    turn.texts.push(text);
    this.expectInput(text, turn.id);
    pi.sendUserMessage(images.length > 0 ? userContent(text, images) : text, { deliverAs: "steer" });
    this.deps.ack.complete(msg.seq);
    turn.messages.push(msg);
    turn.seqs.push(...msg.seq);
  }

  private expectInput(text: string, turnId: string): void {
    this.expectedInputs.push({ text, turnId });
  }

  /** `input` handler: only inputs started by bearbot-im pass (7.2). */
  onInput(event: InputEvent): InputEventResult {
    if (event.source === "extension") {
      const index = this.expectedInputs.findIndex((entry) => entry.text === event.text);
      if (index >= 0) {
        this.expectedInputs.splice(index, 1);
        return { action: "continue" };
      }
    }
    this.deps.diagnostics.add({
      source: "input",
      level: "warning",
      message: `拒绝了不是由 bearbot-im 发起的输入（来源 ${event.source}）：请通过 bearbot SDK 的 prompt() 发起回合`,
      detail: event.text.slice(0, 500),
    });
    return { action: "handled" };
  }

  /**
   * Called first in before_agent_start. Returns true for a run of the current turn: a user run whose prompt is one
   * of the texts dispatched for that turn (dispatch turns off prompt template expansion, so the prompt equals the
   * dispatched text). Any other run is aborted, like a foreign input: a run bearbot-im did not start (for example
   * pi.sendMessage with triggerTurn), a run of a turn the watchdog already ended, or a run whose text another
   * extension's input handler rewrote.
   */
  claimRun(event: BeforeAgentStartEvent, ctx: ExtensionContext): boolean {
    const turn = this.current;
    if (turn && turn.phase === "dispatched" && event.messageType === "user" && turn.texts.includes(event.prompt)) return true;
    this.foreignRun = true;
    ctx.abort();
    const late = event.messageType === "user" ? this.abandoned.indexOf(event.prompt) : -1;
    if (late >= 0) {
      this.abandoned.splice(late, 1);
      this.deps.diagnostics.add({ source: "input", level: "warning", message: "中止了一轮在超时结束之后才开始的运行" });
      return false;
    }
    const what = event.messageType === "custom" ? `自定义消息 ${event.message.customType}` : "用户消息";
    this.deps.diagnostics.add({
      source: "input",
      level: "warning",
      message: `中止了不是由 bearbot-im 发起的一轮（${what}）：请通过 bearbot SDK 的 prompt() 发起回合`,
    });
    return false;
  }

  onAgentStart(): void {
    if (this.foreignRun) return;
    const turn = this.current;
    if (turn && turn.phase === "dispatched") {
      turn.phase = "running";
      this.stopWatchdog(turn);
    }
  }

  onAssistantMessage(message: AssistantMessage): void {
    const turn = this.current;
    if (this.foreignRun || !turn || turn.phase === "preparing" || turn.phase === "ending") return;
    turn.lastAssistant = message;
    if (turn.replyMode !== "each" || turn.origin === null) return;
    if (message.stopReason === "error" || message.stopReason === "aborted") return;
    const text = assistantText(message.content);
    if (!shouldSendReply(text)) return;
    const origin = turn.origin;
    turn.eachChain = turn.eachChain.then(() => this.sendThroughOutbound({ target: origin, markdown: text.trim(), files: [], turn: this.turnInfo(turn) }));
  }

  async onAgentSettled(event: AgentSettledEvent): Promise<void> {
    if (this.foreignRun) {
      this.foreignRun = false;
      this.kick();
      return;
    }
    const turn = this.current;
    if (!turn || turn.phase === "preparing" || turn.phase === "ending") return;
    const last = turn.lastAssistant;
    const error = !event.aborted && last?.stopReason === "error" ? (last.errorMessage ?? "助手消息以 error 结束，没有错误信息") : undefined;
    await this.finish(turn, { aborted: event.aborted, ...(error === undefined ? {} : { error }) });
  }

  /**
   * `<runtime>`/`send_user_message` error forwarded by the daemon: pi rejected a dispatched prompt before agent_start.
   * While texts of turns the watchdog ended are pending, the error belongs to the oldest of them.
   */
  onDispatchRejected(error: string): boolean {
    if (this.abandoned.length > 0) {
      this.abandoned.shift();
      this.deps.diagnostics.add({
        source: "turn",
        level: "warning",
        message: "pi 拒绝了一轮已经超时结束的回合的输入，当前回合不受影响",
        detail: error,
      });
      return true;
    }
    const turn = this.current;
    if (!turn || turn.phase !== "dispatched") return false;
    this.finish(turn, { aborted: false, error: `pi 拒绝了这一轮：${error}` }).then(undefined, (failure: unknown) => {
      throw failure;
    });
    return true;
  }

  private startWatchdog(turn: ActiveTurn): void {
    turn.watchdog = setInterval(() => {
      if (turn.phase !== "dispatched") {
        this.stopWatchdog(turn);
        return;
      }
      const ctx = this.deps.ctx();
      turn.idleTicks = ctx && ctx.isIdle() ? turn.idleTicks + 1 : 0;
      if (turn.idleTicks < DISPATCH_IDLE_LIMIT) return;
      this.stopWatchdog(turn);
      this.abandoned.push(...turn.texts);
      this.finish(turn, { aborted: false, error: `pi 在 ${DISPATCH_IDLE_LIMIT} 秒内没有开始这一轮，也没有报告错误` }).then(undefined, (failure: unknown) => {
        throw failure;
      });
    }, IDLE_CHECK_MS);
    turn.watchdog.unref();
  }

  private stopWatchdog(turn: ActiveTurn): void {
    if (turn.watchdog) clearInterval(turn.watchdog);
    turn.watchdog = null;
  }

  private startTyping(turn: ActiveTurn): void {
    const origin = turn.origin;
    if (origin === null) return;
    const send = () => {
      if (!this.deps.gateway.connected) return;
      this.deps.gateway.request("typing", { target: origin }).then(undefined, (error: Error) => {
        if (turn.typingFailed) return;
        turn.typingFailed = true;
        this.deps.diagnostics.add({ source: "typing", level: "warning", message: `向 ${origin} 发送输入状态失败`, detail: error.message });
      });
    };
    send();
    turn.typingTimer = setInterval(send, TYPING_INTERVAL_MS);
    turn.typingTimer.unref();
  }

  private notifyGateway(turn: ActiveTurn | null): void {
    const params: TurnNotification = turn
      ? { active: true, origin: turn.origin, role: turn.role, startedAt: turn.startedAt }
      : { active: false };
    this.deps.gateway.notify("turn", params);
  }

  /** After a gateway reconnect, tells the new connection about the turn in progress. */
  renotify(): void {
    if (this.current && this.current.phase !== "preparing") this.notifyGateway(this.current);
  }

  private watchOutbound(run: OutboundRun, target: string): void {
    run.result.then(undefined, (error: Error) => {
      this.deps.diagnostics.add({ source: "outbound", level: "error", message: `发送到 ${target} 失败`, detail: error.message });
    });
  }

  private async sendThroughOutbound(reply: OutboundReply): Promise<void> {
    const run = this.deps.outbound.run(reply, this.deps.sendReply);
    this.watchOutbound(run, reply.target);
    await run.progressed;
  }

  /** A failed end rejects; the callers of finish() let it end the process, and the daemon compares on exit. */
  private async endClassifierTurn(turn: ActiveTurn): Promise<void> {
    const end = await this.deps.agent.classifierTurnEnd(turn.id);
    if (end.restored || (end.detail !== undefined && end.detail !== "")) {
      // The daemon notifies the admins itself (DESIGN 7.8).
      this.deps.diagnostics.add({
        source: "classifier-restore",
        level: "error",
        message: end.restored
          ? "本轮（member）结束时 daemon 恢复了被修改的 CLASSIFIER.md"
          : "本轮（member）中 CLASSIFIER.md 有变化：回合开始时它不是普通文件，daemon 没有恢复，只报告了这次变化",
        ...(end.detail === undefined ? {} : { detail: end.detail }),
        notifyAdmins: false,
      });
    }
  }

  private async finish(turn: ActiveTurn, result: { aborted: boolean; error?: string }): Promise<void> {
    turn.phase = "ending";
    this.stopWatchdog(turn);
    if (turn.typingTimer) clearInterval(turn.typingTimer);
    turn.typingTimer = null;
    const text = turn.lastAssistant ? assistantText(turn.lastAssistant.content) : "";
    const info = this.turnInfo(turn);

    if (turn.classifier) await this.endClassifierTurn(turn);

    if (result.error !== undefined) {
      this.deps.diagnostics.add({
        source: "turn",
        level: "error",
        message: `回合出错（本轮角色 ${turn.role}，回复目标 ${turn.origin ?? "无"}）`,
        detail: result.error,
      });
      if (turn.origin !== null) {
        const notice = turn.role === "member" ? this.deps.errorNotice : `处理时出错：${result.error}`;
        try {
          await this.deps.gateway.post({ target: turn.origin, markdown: notice });
        } catch (error) {
          this.deps.diagnostics.add({ source: "outbound", level: "error", message: `向 ${turn.origin} 发送错误提示失败`, detail: (error as Error).message });
        }
      }
    } else if (!result.aborted && turn.replyMode === "final" && turn.origin !== null && shouldSendReply(text)) {
      await this.sendThroughOutbound({ target: turn.origin, markdown: text.trim(), files: [], turn: info });
    }
    await turn.eachChain;

    const record = this.deps.rollback.record;
    if (record && rollbackAckAllowed({
      role: turn.role,
      viaPrompt: turn.viaPrompt,
      allSendersAdmins: turn.messages.every((message) => message.sender.role === "admin"),
      messageCount: turn.messages.length,
      aborted: result.aborted,
      error: result.error,
    })) {
      try {
        await this.deps.agent.rollbackAck(record.id);
        this.deps.rollback.clear(record.id);
      } catch (error) {
        this.deps.diagnostics.add({ source: "rollback", level: "warning", message: `回滚记录 ${record.id} 的确认没有交给 daemon`, detail: (error as Error).message });
      }
    }

    const ended: TurnResult = { text, aborted: result.aborted, ...(result.error === undefined ? {} : { error: result.error }) };
    for (const entry of turn.listenersStarted ? [...this.deps.turnEndListeners.list()] : []) {
      try {
        entry.fn(info, ended);
      } catch (error) {
        this.deps.diagnostics.add({
          source: `listener:${entry.owner.name}`,
          level: "error",
          message: `扩展 ${entry.owner.name} 的 onTurnEnd 监听函数抛出异常`,
          detail: errorDetail(error),
        });
      }
    }
    for (let i = this.expectedInputs.length - 1; i >= 0; i -= 1) {
      if (this.expectedInputs[i].turnId === turn.id) this.expectedInputs.splice(i, 1);
    }
    this.current = null;
    this.notifyGateway(null);
    this.kick();
  }

  status(): string[] {
    const turn = this.current;
    const lines: string[] = [];
    if (turn) {
      lines.push(`当前回合：${turn.kind}${turn.systemKind ? `（${turn.systemKind}）` : ""}，角色 ${turn.role}，目标 ${turn.origin ?? "无"}，阶段 ${turn.phase}，开始于 ${turn.startedAt}`);
    } else {
      lines.push(this.foreignRun ? "当前回合：无（正在中止一轮不是由 bearbot-im 发起的运行）" : "当前回合：无");
    }
    const queued = this.deps.queue.summary();
    lines.push(`派发队列：${queued.length} 项`);
    for (const item of queued.slice(0, 20)) lines.push(`  ${item}`);
    return lines;
  }
}
