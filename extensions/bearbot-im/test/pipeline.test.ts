import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterAll, afterEach, describe, expect, test } from "vitest";
import type { InboundMessage, InboundMiddleware, Next, OutboundMiddleware, OutboundReply } from "../../../sdk/index.ts";
import { Diagnostics, diagnosticsFile } from "../src/diagnostics.ts";
import { TurnQueue } from "../src/dispatch.ts";
import { GatewayClient } from "../src/gateway-client.ts";
import { AckTracker, InboundPipeline, mergeMessages, OutboundPipeline, Registry, type Subscriber } from "../src/pipeline.ts";
import { messageFromWire } from "../src/render.ts";
import { removeTempDirs, tempDir, wire } from "./helpers.ts";

afterAll(removeTempDirs);

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

interface Setup {
  ack: AckTracker;
  queue: TurnQueue;
  diagnostics: Diagnostics;
  file: string;
  subscribers: Registry<Subscriber>;
  middleware: Registry<InboundMiddleware>;
  pipeline: InboundPipeline;
}

const pipelines: InboundPipeline[] = [];

afterEach(() => {
  for (const pipeline of pipelines.splice(0)) pipeline.close();
});

function setup(options: { holdWarnAfterMs?: number; observeLimit?: number } = {}): Setup {
  const workspace = tempDir("bbim-pipe-");
  const file = diagnosticsFile(workspace);
  const diagnostics = new Diagnostics(file);
  const ack = new AckTracker();
  const queue = new TurnQueue(options.observeLimit ?? 30, ack);
  const subscribers = new Registry<Subscriber>();
  const middleware = new Registry<InboundMiddleware>();
  // Not started: ctl.reply is not used here.
  const gateway = new GatewayClient(join(workspace, "gateway.sock"), () => ack.acked);
  const pipeline = new InboundPipeline({
    subscribers,
    middleware,
    router: queue,
    ack,
    diagnostics,
    gateway,
    holdWarnAfterMs: options.holdWarnAfterMs ?? 600_000,
  });
  pipelines.push(pipeline);
  return { ack, queue, diagnostics, file, subscribers, middleware, pipeline };
}

const owner = (name: string) => ({ name, generation: 1 });

function queuedTexts(queue: TurnQueue): string[] {
  const texts: string[] = [];
  for (let batch = queue.takeNext(); batch; batch = queue.takeNext()) {
    if (batch.kind === "message") texts.push(...batch.messages.map((message) => message.text));
  }
  return texts;
}

function fileRecords(file: string): { source: string; level: string; message: string }[] {
  return readFileSync(file, "utf8").trim().split("\n").filter((line) => line !== "").map((line) => JSON.parse(line));
}

describe("AckTracker", () => {
  test("acks the highest seq up to which every received message is complete", () => {
    const ack = new AckTracker();
    const advanced: number[] = [];
    ack.onAdvance((seq) => advanced.push(seq));
    for (const seq of [3, 5, 8]) expect(ack.receive(seq)).toBe(true);
    ack.complete([5]);
    expect(ack.acked).toBeNull();
    ack.complete([3]);
    expect(ack.acked).toBe(5);
    expect(ack.receive(5)).toBe(false);
    expect(ack.receive(8)).toBe(false);
    ack.complete([8]);
    expect(ack.acked).toBe(8);
    expect(advanced).toEqual([5, 8]);
    expect(ack.incomplete()).toEqual([]);
  });
});

describe("inbound pipeline", () => {
  test("subscribers run before middleware and get a read-only copy", async () => {
    const s = setup();
    const events: string[] = [];
    s.subscribers.add((msg) => {
      events.push(`subscriber:${msg.text}`);
      (msg as InboundMessage).text = "changed";
    }, 0, owner("watcher"));
    s.middleware.add((msg, next) => {
      events.push(`middleware:${msg.text}`);
      next(msg);
    }, 0, owner("mw"));
    s.pipeline.receive(wire({ text: "hello" }));
    await sleep(10);
    expect(events).toEqual(["subscriber:hello", "middleware:hello"]);
    expect(queuedTexts(s.queue)).toEqual(["hello"]);
    expect(s.diagnostics.pending().map((record) => record.source)).toEqual(["subscriber:watcher"]);
  });

  test("middleware runs by order, equal order keeps registration order", async () => {
    const s = setup();
    const calls: string[] = [];
    const mark = (name: string): InboundMiddleware => (msg, next) => {
      calls.push(name);
      msg.text += `>${name}`;
      next(msg);
    };
    s.middleware.add(mark("a20"), 20, owner("a"));
    s.middleware.add(mark("b10"), 10, owner("b"));
    s.middleware.add(mark("c10"), 10, owner("c"));
    s.middleware.add(mark("d-5"), -5, owner("d"));
    s.pipeline.receive(wire({ text: "x" }));
    await sleep(10);
    expect(calls).toEqual(["d-5", "b10", "c10", "a20"]);
    expect(queuedTexts(s.queue)).toEqual(["x>d-5>b10>c10>a20"]);
  });

  test("merge carries every seq and completes them together", async () => {
    const s = setup();
    const held: InboundMessage[] = [];
    let latestNext: Next | null = null;
    s.middleware.add((msg, next, ctl) => {
      held.push(msg);
      latestNext = next;
      if (held.length === 3) latestNext(ctl.merge(held));
    }, 10, owner("debounce"));
    const first = wire({ text: "一", trigger: false, time: "2026-10-26T14:00:01+08:00" });
    const second = wire({ text: "二", trigger: false, time: "2026-10-26T14:00:02+08:00" });
    const third = wire({ text: "三", trigger: false, time: "2026-10-26T14:00:03+08:00" });
    s.pipeline.receive(first);
    s.pipeline.receive(second);
    await sleep(10);
    expect(s.ack.acked).toBeNull();
    s.pipeline.receive(third);
    await sleep(10);
    expect(s.ack.acked).toBe(third.seq[0]);
    const observed = s.queue.takeObserved("telegram:-100123");
    expect(observed).toHaveLength(1);
    expect(observed[0].seq).toEqual([first.seq[0], second.seq[0], third.seq[0]]);
    expect(observed[0].text).toBe("一\n二\n三");
  });

  test("merge keeps the lowest role and joins in time order", () => {
    const admin = messageFromWire(wire({ text: "admin", time: "2026-10-26T14:00:05+08:00", sender: { id: "telegram:1", name: "A", role: "admin", isBot: false } }));
    const member = messageFromWire(wire({ text: "member", time: "2026-10-26T14:00:01+08:00", sender: { id: "telegram:2", name: "M", role: "member", isBot: false } }));
    const merged = mergeMessages([admin, member]);
    expect(merged.text).toBe("member\nadmin");
    expect(merged.sender.role).toBe("member");
    expect(merged.seq).toEqual([...member.seq, ...admin.seq].sort((a, b) => a - b));
  });

  test("drop completes the message without routing it", async () => {
    const s = setup();
    s.middleware.add((msg, next, ctl) => {
      if (msg.text === "spam") ctl.drop(msg, "垃圾消息");
      else next(msg);
    }, 10, owner("filter"));
    const spam = wire({ text: "spam" });
    s.pipeline.receive(spam);
    await sleep(10);
    expect(s.ack.acked).toBe(spam.seq[0]);
    expect(s.queue.size).toBe(0);
    expect(s.pipeline.drops.at(-1)).toMatchObject({ seq: spam.seq, by: "filter", reason: "垃圾消息" });
  });

  test("a held message blocks the ack until it is released by unsubscribing", async () => {
    const s = setup();
    const unsubscribe = s.middleware.add((msg, next) => {
      if (msg.text !== "hold") next(msg);
    }, 10, owner("holder"));
    const a = wire({ text: "a", trigger: false });
    const b = wire({ text: "hold", trigger: false });
    const c = wire({ text: "c", trigger: false });
    for (const message of [a, b, c]) s.pipeline.receive(message);
    await sleep(10);
    expect(s.ack.acked).toBe(a.seq[0]);
    expect(s.pipeline.held().map((item) => item.seq)).toEqual(b.seq);
    unsubscribe();
    expect(s.ack.acked).toBe(c.seq[0]);
    expect(s.queue.takeObserved("telegram:-100123").map((message) => message.text)).toEqual(["a", "c", "hold"]);
  });

  test("a held message continues once when the middleware calls next after its registration was removed", async () => {
    const s = setup();
    // The debounce middleware of DESIGN 8.4, with a shorter delay.
    const pending = new Map<string, { msgs: InboundMessage[]; next: Next; timer: NodeJS.Timeout }>();
    s.middleware.add((msg, next, ctl) => {
      if (!msg.trigger) return next(msg);
      const key = `${msg.thread}|${msg.sender.id}`;
      const entry = pending.get(key) ?? { msgs: [], next, timer: undefined as unknown as NodeJS.Timeout };
      clearTimeout(entry.timer);
      entry.msgs.push(msg);
      entry.next = next;
      entry.timer = setTimeout(() => {
        pending.delete(key);
        entry.next(ctl.merge(entry.msgs));
      }, 300);
      pending.set(key, entry);
    }, 100, owner("debounce"));
    const a = wire({ text: "a", time: "2026-10-26T14:00:01+08:00" });
    const b = wire({ text: "b", time: "2026-10-26T14:00:02+08:00" });
    s.pipeline.receive(a);
    s.pipeline.receive(b);
    await sleep(50);
    expect(s.pipeline.held().map((item) => item.seq)).toEqual([...a.seq, ...b.seq]);
    // What Bridge.onSessionShutdown does after a runtime replacement; the old runtime's timer keeps running.
    s.middleware.removeGeneration(1);
    expect(s.ack.acked).toBeNull();
    await sleep(400);
    expect(pending.size).toBe(0);
    expect(queuedTexts(s.queue)).toEqual(["a", "b"]);
    expect(s.pipeline.held()).toEqual([]);
  });

  test("a late drop from a removed middleware leaves the message with the middleware that holds it now", async () => {
    const s = setup();
    const late: (() => void)[] = [];
    const unsubscribe = s.middleware.add((msg, _next, ctl) => {
      late.push(() => ctl.drop(msg, "太晚了"));
    }, 10, owner("first"));
    s.middleware.add(() => {}, 20, owner("second"));
    const message = wire({ text: "held" });
    s.pipeline.receive(message);
    await sleep(10);
    unsubscribe();
    expect(s.pipeline.held()).toMatchObject([{ seq: message.seq[0], stage: "middleware:second" }]);
    expect(late).toHaveLength(1);
    late[0]();
    expect(s.pipeline.held()).toMatchObject([{ seq: message.seq[0], stage: "middleware:second" }]);
    expect(s.pipeline.drops).toEqual([]);
    expect(s.ack.acked).toBeNull();
  });

  test("holding longer than hold_warn_after gives a warning diagnostic", async () => {
    const s = setup({ holdWarnAfterMs: 100 });
    s.middleware.add(() => {}, 10, owner("sleepy"));
    const message = wire({ text: "held" });
    s.pipeline.receive(message);
    await sleep(300);
    const warnings = fileRecords(s.file).filter((record) => record.source === "hold:sleepy");
    expect(warnings).toHaveLength(1);
    expect(warnings[0].level).toBe("warning");
    expect(warnings[0].message).toContain(`seq ${message.seq[0]}`);
  });

  test("a throwing middleware is skipped with the message as it was before it", async () => {
    const s = setup();
    s.middleware.add((msg) => {
      msg.text = "broken";
      throw new Error("sync failure");
    }, 10, owner("sync-thrower"));
    s.middleware.add(async (msg) => {
      msg.text = "also broken";
      await sleep(5);
      throw new Error("async failure");
    }, 20, owner("async-thrower"));
    s.middleware.add((msg, next) => {
      msg.text += "!";
      next(msg);
    }, 30, owner("ok"));
    s.pipeline.receive(wire({ text: "original" }));
    await sleep(50);
    expect(queuedTexts(s.queue)).toEqual(["original!"]);
    const errors = fileRecords(s.file).filter((record) => record.level === "error").map((record) => record.source);
    expect(errors).toEqual(["middleware:sync-thrower", "middleware:async-thrower"]);
  });

  test("a middleware that throws after passing the message on does not duplicate it", async () => {
    const s = setup();
    s.middleware.add((msg, next) => {
      next(msg);
      throw new Error("after next");
    }, 10, owner("late-thrower"));
    s.pipeline.receive(wire({ text: "once" }));
    await sleep(10);
    expect(queuedTexts(s.queue)).toEqual(["once"]);
    expect(s.diagnostics.pending().map((record) => record.source)).toEqual(["middleware:late-thrower"]);
  });

  test("duplicate seqs after a reconnect are ignored", async () => {
    const s = setup();
    const message = wire({ text: "dup" });
    s.pipeline.receive(message);
    s.pipeline.receive({ ...message });
    await sleep(10);
    expect(queuedTexts(s.queue)).toEqual(["dup"]);
  });

  test("observed buffer keeps the newest observe_limit messages per thread", async () => {
    const s = setup({ observeLimit: 2 });
    for (const text of ["1", "2", "3"]) s.pipeline.receive(wire({ text, trigger: false }));
    s.pipeline.receive(wire({ text: "other", trigger: false, thread: "telegram:-100999" }));
    await sleep(10);
    expect(s.queue.takeObserved("telegram:-100123").map((message) => message.text)).toEqual(["2", "3"]);
    expect(s.queue.takeObserved("telegram:-100999").map((message) => message.text)).toEqual(["other"]);
  });
});

describe("dispatch queue batching", () => {
  test("the earliest message picks the thread and every queued message of that thread joins", () => {
    const ack = new AckTracker();
    const queue = new TurnQueue(30, ack);
    const a1 = messageFromWire(wire({ text: "a1", thread: "t:a" }));
    const b1 = messageFromWire(wire({ text: "b1", thread: "t:b" }));
    const a2 = messageFromWire(wire({ text: "a2", thread: "t:a" }));
    const observedA = messageFromWire(wire({ text: "oa", thread: "t:a", trigger: false }));
    for (const message of [a1, b1, a2, observedA]) {
      ack.receive(message.seq[0]);
      queue.route(message);
    }
    const first = queue.takeNext();
    expect(first?.kind).toBe("message");
    if (first?.kind !== "message") return;
    expect(first.thread).toBe("t:a");
    expect(first.messages.map((message) => message.text)).toEqual(["a1", "a2"]);
    expect(first.observed.map((message) => message.text)).toEqual(["oa"]);
    const second = queue.takeNext();
    expect(second?.kind === "message" && second.messages.map((message) => message.text)).toEqual(["b1"]);
    expect(queue.takeNext()).toBeNull();
  });
});

describe("outbound pipeline", () => {
  function outboundSetup() {
    const workspace = tempDir("bbim-out-");
    const diagnostics = new Diagnostics(diagnosticsFile(workspace));
    const registry = new Registry<OutboundMiddleware>();
    const pipeline = new OutboundPipeline(registry, diagnostics, 100);
    // Not started; every reply in these tests is dropped by a middleware before it would be posted.
    const gateway = new GatewayClient(join(workspace, "gateway.sock"), () => null);
    const send = (reply: OutboundReply) => gateway.post({ target: reply.target, markdown: reply.markdown });
    return { diagnostics, registry, pipeline, send };
  }

  const reply = (markdown: string): OutboundReply => ({ target: "telegram:1", markdown, files: [], turn: null });

  test("runs by order and reports a drop", async () => {
    const o = outboundSetup();
    o.registry.add((r, next) => next({ ...r, markdown: `${r.markdown}+20` }), 20, owner("second"));
    o.registry.add((r, next) => next({ ...r, markdown: `${r.markdown}+10` }), 10, owner("first"));
    let seen = "";
    o.registry.add((r, _next, ctl) => {
      seen = r.markdown;
      ctl.drop("测试丢弃");
    }, 30, owner("dropper"));
    const run = o.pipeline.run(reply("x"), o.send);
    await run.progressed;
    expect(await run.result).toEqual({ kind: "dropped", by: "dropper", reason: "测试丢弃" });
    expect(seen).toBe("x+10+20");
  });

  test("an exception skips the middleware with the reply as it was before it", async () => {
    const o = outboundSetup();
    o.registry.add((r) => {
      r.markdown = "mangled";
      throw new Error("boom");
    }, 10, owner("thrower"));
    let seen = "";
    o.registry.add((r, _next, ctl) => {
      seen = r.markdown;
      ctl.drop("done");
    }, 20, owner("dropper"));
    const run = o.pipeline.run(reply("clean"), o.send);
    expect((await run.result).kind).toBe("dropped");
    expect(seen).toBe("clean");
    expect(o.diagnostics.pending().map((record) => record.source)).toEqual(["outbound-middleware:thrower"]);
  });

  test("a held reply stops the wait, warns after hold_warn_after and continues when the holder is removed", async () => {
    const o = outboundSetup();
    const unsubscribe = o.registry.add(() => {}, 10, owner("holder"));
    o.registry.add((_r, _next, ctl) => ctl.drop("released"), 20, owner("dropper"));
    const run = o.pipeline.run(reply("wait"), o.send);
    await run.progressed;
    expect(o.pipeline.held()).toEqual([{ target: "telegram:1", middleware: "holder" }]);
    await sleep(200);
    expect(o.diagnostics.pending().map((record) => record.source)).toEqual(["hold:holder"]);
    unsubscribe();
    expect(await run.result).toEqual({ kind: "dropped", by: "dropper", reason: "released" });
  });
});
