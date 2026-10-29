import type {
  InboundMessage,
  InboundMiddleware,
  MiddlewareControl,
  OutboundMiddleware,
  OutboundReply,
} from "../../../sdk/index.ts";
import type { Diagnostics } from "./diagnostics.ts";
import type { GatewaySender, WireMessage } from "./gateway-client.ts";
import { cloneMessage, messageFromWire } from "./render.ts";

// Inbound path of DESIGN 7.2 / 8.3: subscribers, ordered middleware, routing, and the contiguous ack.

/**
 * What bearbot-im tells the gateway (DESIGN 6.8): `acked` is the highest seq up to which every received message is
 * complete; `done` are the completed seqs above it, which the gateway must not send again.
 */
export interface AckState {
  acked: number | null;
  done: number[];
}

/** Tracks received seq numbers and which of them are complete. */
export class AckTracker {
  private readonly order: number[] = [];      // received, not yet acked, ascending
  private readonly known = new Set<number>();
  private readonly done = new Set<number>();
  private ackedSeq: number | null = null;
  private readonly listeners = new Set<(state: AckState) => void>();

  get acked(): number | null {
    return this.ackedSeq;
  }

  state(): AckState {
    return { acked: this.ackedSeq, done: this.order.filter((seq) => this.done.has(seq)) };
  }

  /** Called after every complete() that completed a received seq. */
  onChange(listener: (state: AckState) => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  /** Registers a received seq; returns false for a duplicate (already received or at or below the ack point). */
  receive(seq: number): boolean {
    if (!Number.isInteger(seq)) throw new Error(`seq 不是整数：${seq}`);
    if (this.ackedSeq !== null && seq <= this.ackedSeq) return false;
    if (this.known.has(seq)) return false;
    this.known.add(seq);
    let index = this.order.length;
    while (index > 0 && this.order[index - 1] > seq) index -= 1;
    this.order.splice(index, 0, seq);
    return true;
  }

  complete(seqs: readonly number[]): void {
    let changed = false;
    for (const seq of seqs) {
      if (this.known.has(seq) && !this.done.has(seq)) {
        this.done.add(seq);
        changed = true;
      }
    }
    if (!changed) return;
    while (this.order.length > 0 && this.done.has(this.order[0])) {
      const seq = this.order.shift() as number;
      this.done.delete(seq);
      this.known.delete(seq);
      this.ackedSeq = seq;
    }
    const state = this.state();
    for (const listener of this.listeners) listener(state);
  }

  isComplete(seq: number): boolean {
    return !this.known.has(seq) || this.done.has(seq);
  }

  /** Received seqs that are not complete yet. */
  incomplete(): number[] {
    return this.order.filter((seq) => !this.done.has(seq));
  }
}

export interface Owner {
  name: string;
  generation: number;
}

export interface Registration<T> {
  readonly fn: T;
  readonly order: number;
  readonly index: number;
  readonly owner: Owner;
  active: boolean;
}

/** Ordered registrations; equal `order` keeps registration order. */
export class Registry<T> {
  private entries: Registration<T>[] = [];
  private counter = 0;
  private readonly removedListeners = new Set<(entry: Registration<T>) => void>();

  add(fn: T, order: number, owner: Owner): () => void {
    if (!Number.isFinite(order)) throw new Error(`order 必须是有限的数字：${order}`);
    const entry: Registration<T> = { fn, order, index: this.counter++, owner, active: true };
    this.entries.push(entry);
    this.entries.sort((a, b) => a.order - b.order || a.index - b.index);
    return () => this.remove(entry);
  }

  remove(entry: Registration<T>): void {
    if (!entry.active) return;
    entry.active = false;
    this.entries = this.entries.filter((candidate) => candidate !== entry);
    for (const listener of this.removedListeners) listener(entry);
  }

  removeGeneration(generation: number): void {
    for (const entry of [...this.entries]) if (entry.owner.generation === generation) this.remove(entry);
  }

  onRemoved(listener: (entry: Registration<T>) => void): void {
    this.removedListeners.add(listener);
  }

  list(): readonly Registration<T>[] {
    return this.entries;
  }

  /** First active registration that sorts after `after` (which may have been removed already). */
  firstAfter(after: Registration<T> | null): Registration<T> | undefined {
    if (after === null) return this.entries[0];
    return this.entries.find((entry) => entry.order > after.order || (entry.order === after.order && entry.index > after.index));
  }
}

export type Subscriber = (msg: Readonly<InboundMessage>) => void | Promise<void>;

/** Where messages go after the last middleware. */
export interface RouteTarget {
  route(msg: InboundMessage): void;
}

export interface DropRecord {
  time: number;
  seq: number[];
  thread: string;
  by: string;
  reason: string;
  text: string;
}

type Stage = { kind: "subscriber" | "middleware"; name: string; entry: Registration<InboundMiddleware> | null; since: number; warned: boolean };
/** One message given to one middleware. `moved` is set once the message left it (next, drop, exception, removal). */
type Step = { entry: Registration<InboundMiddleware>; snapshot: InboundMessage; moved: boolean };

const DROPS_KEPT = 20;

function deepFreeze<T>(value: T): T {
  if (value !== null && typeof value === "object" && !Object.isFrozen(value)) {
    Object.freeze(value);
    for (const key of Object.keys(value)) deepFreeze((value as Record<string, unknown>)[key]);
  }
  return value;
}

function errorDetail(error: unknown): string {
  return error instanceof Error ? (error.stack ?? error.message) : String(error);
}

function isPromise(value: unknown): value is Promise<unknown> {
  return value !== null && typeof value === "object" && typeof (value as { then?: unknown }).then === "function";
}

function timeOf(message: InboundMessage): number {
  const value = Date.parse(message.time);
  if (Number.isNaN(value)) throw new Error(`消息时间无法解析：${message.time}`);
  return value;
}

/**
 * ctl.merge(): one message carrying every seq. Texts, attachments and context are joined in time order; the
 * other fields come from the newest message. Every message must have the same sender with the same role, so the
 * merged text is never put under the name or the role of someone who did not write it.
 */
export function mergeMessages(msgs: InboundMessage[]): InboundMessage {
  if (msgs.length === 0) throw new Error("merge 需要至少一条消息");
  const senders = [...new Set(msgs.map((message) => `${message.sender.id}（${message.sender.role}）`))];
  if (senders.length > 1) throw new Error(`merge 只能合并同一个发送者、同一角色的消息，收到了 ${senders.join("、")} 的消息`);
  const sorted = msgs
    .map((message, position) => ({ message, position }))
    .sort((a, b) => timeOf(a.message) - timeOf(b.message) || a.position - b.position)
    .map((item) => item.message);
  const newest = sorted[sorted.length - 1];
  const merged = cloneMessage(newest);
  merged.seq = [...new Set(sorted.flatMap((message) => message.seq))].sort((a, b) => a - b);
  merged.text = sorted.map((message) => message.text).filter((text) => text !== "").join("\n");
  merged.attachments = sorted.flatMap((message) => message.attachments.map((attachment) => ({ ...attachment })));
  merged.context = sorted.flatMap((message) => message.context.map((entry) => ({ ...entry })));
  merged.meta = Object.assign({}, ...sorted.map((message) => message.meta));
  merged.trigger = sorted.some((message) => message.trigger);
  merged.mentionsBot = sorted.some((message) => message.mentionsBot);
  merged.edited = sorted.every((message) => message.edited);
  merged.stale = sorted.every((message) => message.stale);
  if (sorted.some((message) => message.steer)) merged.steer = true;
  else delete merged.steer;
  const replyTo = sorted.find((message) => message.replyTo)?.replyTo;
  if (replyTo) merged.replyTo = { ...replyTo };
  else delete merged.replyTo;
  return merged;
}

export interface InboundPipelineOptions {
  subscribers: Registry<Subscriber>;
  middleware: Registry<InboundMiddleware>;
  router: RouteTarget;
  ack: AckTracker;
  diagnostics: Diagnostics;
  gateway: GatewaySender;
  holdWarnAfterMs: number;
}

/** Inbound processing of DESIGN 7.2 steps 1-3 with the middleware semantics of 8.3. */
export class InboundPipeline {
  private readonly stages = new Map<number, Stage>();
  private readonly steps = new Set<Step>();
  private subscriberChain: Promise<void> = Promise.resolve();
  private readonly holdTimer: NodeJS.Timeout;
  readonly drops: DropRecord[] = [];

  constructor(private readonly options: InboundPipelineOptions) {
    options.middleware.onRemoved((entry) => this.release(entry));
    const interval = Math.min(60_000, Math.max(50, Math.floor(options.holdWarnAfterMs / 2)));
    this.holdTimer = setInterval(() => this.checkHolds(), interval);
    this.holdTimer.unref();
  }

  close(): void {
    clearInterval(this.holdTimer);
  }

  /** A message notification from the gateway. Duplicates (redelivery after a reconnect) are ignored. */
  receive(wire: WireMessage): void {
    if (wire.seq.length !== 1) throw new Error(`gateway 消息的 seq 应当只有一个元素：${JSON.stringify(wire.seq)}`);
    if (!this.options.ack.receive(wire.seq[0])) return;
    const message = messageFromWire(wire);
    this.setStage(message.seq, { kind: "subscriber", name: "订阅者", entry: null });
    this.subscriberChain = this.subscriberChain.then(() => this.runSubscribers(message));
  }

  /** Seqs that are held in a subscriber or middleware, with where and since when. */
  held(): { seq: number; stage: string; since: number }[] {
    return [...this.stages.entries()].map(([seq, stage]) => ({ seq, stage: `${stage.kind}:${stage.name}`, since: stage.since }));
  }

  private setStage(seqs: readonly number[], stage: Omit<Stage, "since" | "warned">): void {
    const now = Date.now();
    for (const seq of seqs) {
      if (this.options.ack.isComplete(seq)) continue;
      this.stages.set(seq, { ...stage, since: now, warned: false });
    }
  }

  private clearStage(seqs: readonly number[]): void {
    for (const seq of seqs) this.stages.delete(seq);
  }

  private async runSubscribers(message: InboundMessage): Promise<void> {
    for (const entry of [...this.options.subscribers.list()]) {
      if (!entry.active) continue;
      this.setStage(message.seq, { kind: "subscriber", name: entry.owner.name, entry: null });
      try {
        await entry.fn(deepFreeze(cloneMessage(message)));
      } catch (error) {
        this.options.diagnostics.add({
          source: `subscriber:${entry.owner.name}`,
          level: "error",
          message: `订阅者 ${entry.owner.name} 处理消息 seq ${message.seq.join(",")} 时抛出异常`,
          detail: errorDetail(error),
        });
      }
    }
    this.runFrom(message, null);
  }

  private runFrom(message: InboundMessage, after: Registration<InboundMiddleware> | null): void {
    const entry = this.options.middleware.firstAfter(after);
    if (!entry) {
      this.clearStage(message.seq);
      this.options.router.route(message);
      return;
    }
    const snapshot = cloneMessage(message);
    const step: Step = { entry, snapshot, moved: false };
    this.steps.add(step);
    this.setStage(message.seq, { kind: "middleware", name: entry.owner.name, entry });
    // A step continues once: a late next (for example from a timer after the registration was removed) does nothing.
    const next = (forwarded: InboundMessage) => {
      if (step.moved) return;
      step.moved = true;
      this.steps.delete(step);
      this.runFrom(forwarded, entry);
    };
    const onError = (error: unknown) => this.middlewareFailed(step, error);
    try {
      const result = entry.fn(message, next, this.control(entry, step));
      if (isPromise(result)) result.then(undefined, onError);
    } catch (error) {
      onError(error);
    }
  }

  /** Whether every seq of the step's message still sits in the step's middleware. */
  private stillHeld(step: Step): boolean {
    return step.snapshot.seq.every((seq) => this.stages.get(seq)?.entry === step.entry);
  }

  private middlewareFailed(step: Step, error: unknown): void {
    const name = step.entry.owner.name;
    this.options.diagnostics.add({
      source: `middleware:${name}`,
      level: "error",
      message: `中间件 ${name} 处理消息 seq ${step.snapshot.seq.join(",")} 时抛出异常，消息跳过这个中间件继续处理`,
      detail: errorDetail(error),
    });
    // Continue with the message as it was before this middleware, unless the middleware already passed it on.
    if (!step.moved && this.stillHeld(step)) {
      step.moved = true;
      this.steps.delete(step);
      this.runFrom(step.snapshot, step.entry);
    }
  }

  /** A removed middleware cannot pass on the messages it holds; they continue with the next one. */
  private release(entry: Registration<InboundMiddleware>): void {
    for (const step of [...this.steps]) {
      if (step.entry !== entry) continue;
      this.steps.delete(step);
      const held = !step.moved && this.stillHeld(step);
      step.moved = true;
      if (held) this.runFrom(step.snapshot, entry);
    }
  }

  private control(entry: Registration<InboundMiddleware>, step: Step): MiddlewareControl {
    return {
      drop: (msg, reason) => {
        if (step.moved) return;
        step.moved = true;
        this.steps.delete(step);
        // Only seqs that still sit in this middleware: a message that already went on is not dropped here.
        const seqs = msg.seq.filter((seq) => this.stages.get(seq)?.entry === entry);
        if (seqs.length === 0) return;
        this.clearStage(seqs);
        this.drops.push({
          time: Date.now(),
          seq: seqs,
          thread: msg.thread,
          by: entry.owner.name,
          reason,
          text: msg.text.slice(0, 200),
        });
        if (this.drops.length > DROPS_KEPT) this.drops.shift();
        this.options.ack.complete(seqs);
      },
      merge: (msgs) => mergeMessages(msgs),
      reply: async (msg, text) => {
        await this.options.gateway.post({ target: msg.thread, markdown: text, ...(msg.id === "" ? {} : { replyTo: msg.id }) });
      },
    };
  }

  private checkHolds(): void {
    for (const step of [...this.steps]) if (!step.snapshot.seq.some((seq) => this.stages.get(seq)?.entry === step.entry)) this.steps.delete(step);
    const now = Date.now();
    const byStage = new Map<string, number[]>();
    for (const [seq, stage] of this.stages) {
      if (stage.warned || now - stage.since < this.options.holdWarnAfterMs) continue;
      stage.warned = true;
      const key = `${stage.kind}:${stage.name}`;
      byStage.set(key, [...(byStage.get(key) ?? []), seq]);
    }
    for (const [key, seqs] of byStage) {
      const [kind, name] = [key.slice(0, key.indexOf(":")), key.slice(key.indexOf(":") + 1)];
      const label = kind === "subscriber" ? `订阅者 ${name}` : `中间件 ${name}`;
      this.options.diagnostics.add({
        source: `hold:${name}`,
        level: "warning",
        message: `消息 seq ${seqs.join(",")} 在${label}中停留超过 ${Math.round(this.options.holdWarnAfterMs / 1000)} 秒，没有交给下一步也没有丢弃`,
      });
    }
  }
}

export type OutboundResult =
  | { kind: "sent"; ref: string; reply: OutboundReply }
  | { kind: "dropped"; by: string; reason: string };

export interface OutboundRun {
  /** Settles when the reply was sent or dropped; rejects when the gateway post failed. */
  result: Promise<OutboundResult>;
  /** Resolves when the reply was sent or dropped, or when a middleware holds it. */
  progressed: Promise<void>;
}

export type OutboundSend = (reply: OutboundReply) => Promise<{ ref: string }>;

type OutboundStep = {
  entry: Registration<OutboundMiddleware>;
  snapshot: OutboundReply;
  moved: boolean;
  resume: (reply: OutboundReply) => void;
};

/** Outbound middleware chain (DESIGN 8.3): same rules as inbound, applied to final replies, im_send and post(). */
export class OutboundPipeline {
  private readonly steps = new Set<OutboundStep>();

  constructor(
    private readonly middleware: Registry<OutboundMiddleware>,
    private readonly diagnostics: Diagnostics,
    private readonly holdWarnAfterMs: number,
  ) {
    middleware.onRemoved((entry) => {
      for (const step of [...this.steps]) {
        if (step.entry !== entry) continue;
        this.steps.delete(step);
        if (!step.moved) {
          step.moved = true;
          step.resume(step.snapshot);
        }
      }
    });
  }

  run(reply: OutboundReply, send: OutboundSend): OutboundRun {
    let settle!: (value: OutboundResult) => void;
    let fail!: (error: Error) => void;
    const result = new Promise<OutboundResult>((resolve, reject) => {
      settle = resolve;
      fail = reject;
    });
    let progressedResolve!: () => void;
    const progressed = new Promise<void>((resolve) => {
      progressedResolve = resolve;
    });

    const forward = (current: OutboundReply, after: Registration<OutboundMiddleware> | null): void => {
      const entry = this.middleware.firstAfter(after);
      if (!entry) {
        progressedResolve();
        send(current).then((sent) => settle({ kind: "sent", ref: sent.ref, reply: current }), fail);
        return;
      }
      const step: OutboundStep = {
        entry,
        snapshot: { ...current, files: [...current.files] },
        moved: false,
        resume: (resumed) => forward(resumed, entry),
      };
      this.steps.add(step);
      const move = (): boolean => {
        if (step.moved) return false;
        step.moved = true;
        this.steps.delete(step);
        return true;
      };
      const next = (forwarded: OutboundReply) => {
        if (move()) forward(forwarded, entry);
      };
      const ctl = {
        drop: (reason: string) => {
          if (!move()) return;
          progressedResolve();
          settle({ kind: "dropped", by: entry.owner.name, reason });
        },
      };
      const failed = (error: unknown) => {
        this.diagnostics.add({
          source: `outbound-middleware:${entry.owner.name}`,
          level: "error",
          message: `出站中间件 ${entry.owner.name} 处理发往 ${step.snapshot.target} 的消息时抛出异常，消息跳过这个中间件继续处理`,
          detail: errorDetail(error),
        });
        if (move()) forward(step.snapshot, entry);
      };
      const returned = () => {
        if (step.moved) return;
        // Held: the caller stops waiting; the chain continues whenever the middleware calls next or drop.
        progressedResolve();
        const timer = setTimeout(() => {
          if (step.moved) return;
          this.diagnostics.add({
            source: `hold:${entry.owner.name}`,
            level: "warning",
            message: `发往 ${step.snapshot.target} 的消息在出站中间件 ${entry.owner.name} 中停留超过 ${Math.round(this.holdWarnAfterMs / 1000)} 秒，没有交给下一步也没有丢弃`,
          });
        }, this.holdWarnAfterMs);
        timer.unref();
      };
      try {
        const value = entry.fn(current, next, ctl);
        if (isPromise(value)) value.then(returned, failed);
        else returned();
      } catch (error) {
        failed(error);
      }
    };
    forward(reply, null);
    return { result, progressed };
  }

  held(): { target: string; middleware: string }[] {
    return [...this.steps].filter((step) => !step.moved).map((step) => ({ target: step.snapshot.target, middleware: step.entry.owner.name }));
  }
}
