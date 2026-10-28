import { readFileSync, realpathSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import type { AssistantMessage } from "@earendil-works/pi-ai";
import type {
  AgentSettledEvent,
  BeforeAgentStartEvent,
  BeforeAgentStartEventResult,
  ExtensionAPI,
  ExtensionContext,
  InputEvent,
  InputEventResult,
  MessageEndEvent,
  ProjectTrustEventResult,
  SessionCompactEvent,
  SessionShutdownEvent,
  ToolCallEvent,
  ToolCallEventResult,
} from "@earendil-works/pi-coding-agent";
import type {
  BearbotApi,
  BearbotConnectRequest,
  Binding,
  InboundMessage,
  InboundMiddleware,
  OutboundMiddleware,
  OutboundReply,
} from "../../../sdk/index.ts";
import { AgentClient, type AgentWireState, type BridgeEvent, type IncidentNotice, type ReadyResult, type StatusInfo } from "./agent-client.ts";
import { Btw, parseBtwRequest } from "./btw.ts";
import { Classifier } from "./classifier.ts";
import { type DiagnosticRecord, Diagnostics, diagnosticsFile, formatAdminNotice, formatDiagnosticsForTurn } from "./diagnostics.ts";
import { Dispatcher, type SystemTurnSpec, type TurnEndListener, type TurnStartListener, TurnQueue } from "./dispatch.ts";
import { type ClassifierSettings, GatewayClient, type HelloResult, type PostParams, type WireBinding, type WireMessage } from "./gateway-client.ts";
import { Memory, shouldSteerMemory } from "./memory.ts";
import { AckTracker, InboundPipeline, type Owner, OutboundPipeline, Registry, type Subscriber } from "./pipeline.ts";
import { messageFromWire } from "./render.ts";
import { RollbackState, rollbackReminder, rollbackTurnBody } from "./rollback.ts";
import type { ToolsHost } from "./tools.ts";
import { assertNotTrial, parseTrialFlag } from "./trial.ts";

export const CONNECT_EVENT = "bearbot:connect";

export interface BridgeConfig {
  workspaceDir: string;
  workspaceReal: string;
  gatewaySocket: string;
  agentSocket: string;
  classifierModel: { provider: string; modelId: string };
  classifierTimeoutMs: number;
  rulesMaxBytes: number;
  observeLimit: number;
  holdWarnAfterMs: number;
  errorNotice: string;
  trial: boolean;
  extensionDir: string;
}

function required(env: NodeJS.ProcessEnv, name: string): string {
  const value = env[name];
  if (value === undefined || value === "") throw new Error(`bearbot-im 缺少环境变量 ${name}`);
  return value;
}

function integer(env: NodeJS.ProcessEnv, name: string, min: number): number {
  const value = required(env, name);
  if (!/^\d+$/.test(value)) throw new Error(`环境变量 ${name} 必须是整数，当前为 ${value}`);
  const number = Number(value);
  if (number < min) throw new Error(`环境变量 ${name} 不能小于 ${min}，当前为 ${value}`);
  return number;
}

/** Environment of the agent process (contracts 2.1, 2.2). Every variable is required. */
export function readConfig(env: NodeJS.ProcessEnv, extensionDir: string): BridgeConfig {
  const workspaceDir = required(env, "BEARBOT_WORKSPACE_DIR");
  const model = required(env, "BEARBOT_CLASSIFIER_MODEL");
  const slash = model.indexOf("/");
  if (slash <= 0 || slash === model.length - 1) throw new Error(`BEARBOT_CLASSIFIER_MODEL 应当写成 <provider>/<model>，当前为 ${model}`);
  return {
    workspaceDir,
    workspaceReal: realpathSync(workspaceDir),
    gatewaySocket: required(env, "BEARBOT_GATEWAY_SOCKET"),
    agentSocket: required(env, "BEARBOT_AGENT_SOCKET"),
    classifierModel: { provider: model.slice(0, slash), modelId: model.slice(slash + 1) },
    classifierTimeoutMs: integer(env, "BEARBOT_CLASSIFIER_TIMEOUT", 1),
    rulesMaxBytes: integer(env, "BEARBOT_CLASSIFIER_RULES_MAX_BYTES", 0),
    observeLimit: integer(env, "BEARBOT_OBSERVE_LIMIT", 0),
    holdWarnAfterMs: integer(env, "BEARBOT_HOLD_WARN_AFTER", 1),
    errorNotice: required(env, "BEARBOT_ERROR_NOTICE"),
    trial: parseTrialFlag(env.BEARBOT_TRIAL),
    extensionDir,
  };
}

const CLASSIFIER_BLOCK = /\{\{#classifier-(enabled|disabled)\}\}\n?([\s\S]*?)\{\{\/classifier-\1\}\}\n?/g;

/** Fills the placeholders of prompt/bearbot.md and prompt/classifier.md. */
export function renderPromptTemplate(template: string, vars: Record<string, string>, classifierEnabled: boolean): string {
  let text = template.replace(CLASSIFIER_BLOCK, (_match, which: string, body: string) =>
    (which === "enabled") === classifierEnabled ? body : "");
  for (const [name, value] of Object.entries(vars)) text = text.split(`{{${name}}}`).join(value);
  const left = /\{\{[^}]*\}\}/.exec(text);
  if (left) throw new Error(`提示模板中有没有替换的占位符：${left[0]}`);
  return text;
}

function errorDetail(error: unknown): string {
  return error instanceof Error ? (error.stack ?? error.message) : String(error);
}

const DISPATCH_STATES = new Set<AgentWireState>(["starting", "running"]);

function toBinding(wire: WireBinding): Binding {
  return {
    channel: wire.channel,
    ...(wire.name === null ? {} : { name: wire.name }),
    platform: wire.platform,
    trigger: wire.trigger,
    replyMode: wire.replyMode,
  };
}

/** Module-level state of bearbot-im. It survives pi's runtime replacement; each factory call attaches a new pi. */
export class Bridge {
  readonly config: BridgeConfig;
  readonly diagnostics: Diagnostics;
  readonly memory: Memory;
  readonly ack = new AckTracker();
  readonly gateway: GatewayClient;
  readonly agent: AgentClient;
  readonly subscribers = new Registry<Subscriber>();
  readonly middleware = new Registry<InboundMiddleware>();
  readonly outboundMiddleware = new Registry<OutboundMiddleware>();
  readonly turnStartListeners = new Registry<TurnStartListener>();
  readonly turnEndListeners = new Registry<TurnEndListener>();
  readonly bindingListeners = new Registry<(bindings: Binding[]) => void>();
  readonly queue: TurnQueue;
  readonly dispatcher: Dispatcher;
  readonly pipeline: InboundPipeline;
  readonly outbound: OutboundPipeline;
  readonly classifier: Classifier;
  readonly rollback = new RollbackState();
  /** Skills of the agent process (DESIGN 9.1); bearbot-recovery is only for recovery. */
  readonly skillPaths: string[];
  readonly toolsHost: ToolsHost;
  readonly btw: Btw;

  private generation = 0;
  private readonly staleGenerations = new Set<number>();
  private activePi: ExtensionAPI | null = null;
  private activeCtx: ExtensionContext | null = null;
  private connectionsStarted = false;
  private ready = false;
  private restartPending = false;
  private daemonState: AgentWireState | null = null;
  private daemonReason: string | null = null;
  private bindingList: WireBinding[] = [];
  private bindingsKnown = false;
  private admins: string[] = [];
  private classifierSettings: ClassifierSettings = { enabled: true, rules: [] };
  private self: Record<string, string> = {};
  private lastIncident: IncidentNotice | null = null;
  private lastProblem: string | null = null;
  private confirmedAck: number | null = null;
  private readonly bearbotTemplate: string;
  private readonly promptVars: Record<string, string>;

  constructor(config: BridgeConfig) {
    this.config = config;
    this.diagnostics = new Diagnostics(config.trial ? null : diagnosticsFile(config.workspaceDir));
    this.memory = new Memory(config.workspaceReal);
    this.gateway = new GatewayClient(config.gatewaySocket, () => this.ack.acked);
    this.agent = new AgentClient(config.agentSocket);
    const skillsDir = resolve(config.extensionDir, "..", "..", "skills");
    this.skillPaths = [join(skillsDir, "bearbot-extension-dev"), join(skillsDir, "bearbot-middleware")];

    const profileDir = join(dirname(config.workspaceDir), "profile");
    this.promptVars = {
      WORKSPACE: config.workspaceDir,
      PROFILE_DIR: profileDir,
      RUN_DIR: dirname(config.agentSocket),
      SNAPSHOTS_GIT: join(profileDir, "bearbot", "snapshots.git"),
      SNAPSHOT_WORKTREE: dirname(config.workspaceDir),
    };
    this.bearbotTemplate = readFileSync(join(config.extensionDir, "prompt", "bearbot.md"), "utf8");
    // Both variants are rendered once here, so a broken template fails at load time.
    renderPromptTemplate(this.bearbotTemplate, this.promptVars, true);
    renderPromptTemplate(this.bearbotTemplate, this.promptVars, false);
    const classifierPrompt = renderPromptTemplate(
      readFileSync(join(config.extensionDir, "prompt", "classifier.md"), "utf8"),
      this.promptVars,
      true,
    );
    this.btw = new Btw({
      ctx: () => this.activeCtx,
      instructions: renderPromptTemplate(readFileSync(join(config.extensionDir, "prompt", "btw.md"), "utf8"), this.promptVars, true),
    });

    this.queue = new TurnQueue(config.observeLimit, this.ack);
    this.outbound = new OutboundPipeline(this.outboundMiddleware, this.diagnostics, config.holdWarnAfterMs);
    this.dispatcher = new Dispatcher({
      pi: () => this.activePi,
      ctx: () => this.activeCtx,
      canDispatch: () => this.ready && !this.restartPending && (this.daemonState === null || DISPATCH_STATES.has(this.daemonState)),
      queue: this.queue,
      ack: this.ack,
      gateway: this.gateway,
      agent: this.agent,
      diagnostics: this.diagnostics,
      outbound: this.outbound,
      sendReply: (reply) => this.postReply(reply),
      bindings: () => this.bindingList,
      rollback: this.rollback,
      turnStartListeners: this.turnStartListeners,
      turnEndListeners: this.turnEndListeners,
      errorNotice: config.errorNotice,
      rulesMaxBytes: config.rulesMaxBytes,
    });
    this.pipeline = new InboundPipeline({
      subscribers: this.subscribers,
      middleware: this.middleware,
      router: this.dispatcher,
      ack: this.ack,
      diagnostics: this.diagnostics,
      gateway: this.gateway,
      holdWarnAfterMs: config.holdWarnAfterMs,
    });
    this.classifier = new Classifier({
      settings: () => this.classifierSettings,
      turn: () => {
        const turn = this.dispatcher.turn;
        if (!turn) return null;
        return turn.classifier ?? { id: turn.id, role: turn.role, rules: { kind: "none" }, rulesWarned: false, cache: new Map(), messages: () => [] };
      },
      toolInfo: (name) => this.activePi?.getAllTools().find((tool) => tool.name === name),
      agent: this.agent,
      diagnostics: this.diagnostics,
      systemPrompt: classifierPrompt,
      model: config.classifierModel,
      timeoutMs: config.classifierTimeoutMs,
      workspaceReal: config.workspaceReal,
    });
    this.toolsHost = {
      turn: () => this.dispatcher.turn,
      gateway: this.gateway,
      agent: this.agent,
      sendOutbound: (reply) => this.outbound.run(reply, (current) => this.postReply(current)).result,
      statusText: () => this.statusText(),
      diagnosticsTail: (n) => this.diagnostics.tail(n),
      applyAccepted: () => {
        this.restartPending = true;
      },
    };
  }

  /** A factory call: a new runtime with a new pi. */
  attach(): number {
    this.generation += 1;
    return this.generation;
  }

  projectTrust(cwd: string): ProjectTrustEventResult {
    if (realpathSync(cwd) === this.config.workspaceReal) return { trusted: "yes", remember: false };
    return { trusted: "no" };
  }

  onSessionStart(generation: number, pi: ExtensionAPI, ctx: ExtensionContext): void {
    if (generation !== this.generation) return;
    this.activePi = pi;
    this.activeCtx = ctx;
    // Connections are set up once per process, on the first session_start whatever its reason; later
    // session_start events (runtime replacement, the duplicate emission of RPC mode) only rebind pi.
    if (!this.connectionsStarted) {
      this.connectionsStarted = true;
      if (!this.config.trial) this.startConnections();
    }
    try {
      this.memory.reload();
    } finally {
      this.dispatcher.kick();
    }
  }

  onSessionShutdown(generation: number, _event: SessionShutdownEvent): void {
    if (generation !== this.generation) return;
    this.activePi = null;
    this.activeCtx = null;
    this.staleGenerations.add(generation);
    for (const registry of [this.subscribers, this.middleware, this.outboundMiddleware, this.turnStartListeners, this.turnEndListeners, this.bindingListeners]) {
      (registry as Registry<unknown>).removeGeneration(generation);
    }
  }

  kick(): void {
    this.dispatcher.kick();
  }

  onInput(event: InputEvent): InputEventResult {
    return this.dispatcher.onInput(event);
  }

  onBeforeAgentStart(event: BeforeAgentStartEvent, ctx: ExtensionContext): BeforeAgentStartEventResult | undefined {
    if (!this.dispatcher.claimRun(event, ctx)) return undefined;
    const options = event.systemPromptOptions;
    this.memory.applyToContextFiles(options.contextFiles);
    options.sections.bearbot = this.bearbotSection();
    const pending = this.diagnostics.pending();
    if (pending.length === 0) return undefined;
    this.diagnostics.markDelivered(pending.map((record) => record.id));
    return { message: { customType: "bearbot-diagnostics", content: formatDiagnosticsForTurn(pending), display: true } };
  }

  bearbotSection(): string {
    let text = renderPromptTemplate(this.bearbotTemplate, this.promptVars, this.classifierSettings.enabled);
    const record = this.rollback.record;
    if (record) text = `${text.trimEnd()}\n\n${rollbackReminder(record)}\n`;
    return text;
  }

  onAgentStart(): void {
    this.dispatcher.onAgentStart();
  }

  onMessageEnd(event: MessageEndEvent): void {
    if (event.message.role === "assistant") this.dispatcher.onAssistantMessage(event.message as AssistantMessage);
  }

  onAgentSettled(event: AgentSettledEvent): Promise<void> {
    // pi turns a rejected agent_settled handler into an extension_error and keeps running, so a failed turn end
    // (for example the CLASSIFIER.md end comparison) ends the process here.
    return this.dispatcher.onAgentSettled(event).then(undefined, (error: unknown) => this.fatal("结束回合", error));
  }

  onSessionCompact(event: SessionCompactEvent): void {
    this.memory.reload();
    const turn = this.dispatcher.turn;
    const pi = this.activePi;
    if (turn && pi && !this.dispatcher.busyWithForeignRun && (turn.phase === "dispatched" || turn.phase === "running")
      && shouldSteerMemory(event.willRetry, turn.lastAssistant?.stopReason ?? null)) {
      pi.sendMessage({ customType: "bearbot-memory", content: this.memory.steerText(), display: false }, { deliverAs: "steer" });
    }
    this.dispatcher.kick();
  }

  onToolCall(event: ToolCallEvent, ctx: ExtensionContext): Promise<ToolCallEventResult | undefined> {
    return this.classifier.onToolCall(event, ctx);
  }

  // Connections (normal mode only).

  private startConnections(): void {
    this.diagnostics.setNotifier((record) => this.notifyAdmins(record));
    this.ack.onAdvance((seq) => this.sendAck(seq));
    this.gateway.on("hello", (result, reconnect) => this.onHello(result, reconnect));
    this.gateway.on("message", (wire: WireMessage) => this.pipeline.receive(wire));
    this.gateway.on("bindings", (bindings) => this.setBindings(bindings));
    this.gateway.on("admins", (admins) => {
      this.admins = admins;
    });
    this.gateway.on("classifier", (settings) => {
      this.classifierSettings = settings;
    });
    this.gateway.on("disconnected", () => {
      process.stderr.write("bearbot-im：gateway 连接断开，每秒重连一次\n");
    });
    this.gateway.on("problem", (kind, detail) => this.connectionProblem(kind, detail));
    this.gateway.on("disconnected", () => this.btw.abortAll());
    this.gateway.handle("btw", async (params) => ({ answer: await this.btw.answer(parseBtwRequest(params)) }));
    this.gateway.start();
  }

  private connectionProblem(kind: "connect" | "hello", detail: string): void {
    const key = `${kind}:${detail}`;
    if (key === this.lastProblem) return;
    this.lastProblem = key;
    process.stderr.write(`bearbot-im：gateway ${kind === "hello" ? "拒绝了 hello" : "连接失败"}：${detail}\n`);
    if (kind === "hello") {
      this.diagnostics.add({ source: "gateway", level: "warning", message: "gateway 拒绝了 hello（已有其他连接），每秒重试一次", detail });
    }
  }

  private onHello(result: HelloResult, reconnect: boolean): void {
    this.lastProblem = null;
    this.admins = result.admins;
    this.classifierSettings = result.classifier;
    this.self = result.self;
    if (!this.bindingsKnown || JSON.stringify(result.bindings) !== JSON.stringify(this.bindingList)) this.setBindings(result.bindings);
    if (!reconnect) {
      this.runReady().then(undefined, (error: unknown) => this.fatal("连接 daemon（bridge/ready）", error));
      return;
    }
    this.dispatcher.renotify();
    const acked = this.ack.acked;
    if (acked !== null && (this.confirmedAck === null || acked > this.confirmedAck)) this.sendAck(acked);
  }

  private sendAck(seq: number): void {
    if (!this.gateway.connected) return;
    this.gateway.request("ack", { seq }).then(
      () => {
        if (this.confirmedAck === null || seq > this.confirmedAck) this.confirmedAck = seq;
      },
      (error: Error) => {
        // A lost connection needs nothing here: the next hello carries ackedSeq.
        if (!this.gateway.connected) return;
        this.diagnostics.add({ source: "gateway", level: "warning", message: `ack ${seq} 失败`, detail: error.message });
      },
    );
  }

  private setBindings(bindings: WireBinding[]): void {
    this.bindingList = bindings;
    this.bindingsKnown = true;
    const converted = bindings.map(toBinding);
    for (const entry of [...this.bindingListeners.list()]) {
      try {
        entry.fn(converted.map((binding) => ({ ...binding })));
      } catch (error) {
        this.diagnostics.add({
          source: `listener:${entry.owner.name}`,
          level: "error",
          message: `扩展 ${entry.owner.name} 的 onBindingsChange 监听函数抛出异常`,
          detail: errorDetail(error),
        });
      }
    }
  }

  private async runReady(): Promise<void> {
    let lastEventsProblem: string | null = null;
    await this.agent.openEvents(
      (event) => {
        lastEventsProblem = null;
        this.onBridgeEvent(event);
      },
      (detail) => {
        if (detail === lastEventsProblem) return;
        lastEventsProblem = detail;
        process.stderr.write(`bearbot-im：${detail}\n`);
      },
    );
    const pi = this.activePi;
    if (!pi) throw new Error("bridge/ready 时没有可用的 pi 运行时");
    const result = await this.agent.ready(
      pi.getAllTools().map((tool) => tool.name),
      pi.getCommands().map((command) => command.name),
    );
    await this.handleReady(result);
    this.ready = true;
    this.dispatcher.kick();
  }

  private fatal(context: string, error: unknown): never {
    process.stderr.write(`bearbot-im：${context}失败，进程退出：${errorDetail(error)}\n`);
    process.exit(1);
  }

  private async origin(requesters: string[] | null, thread: string | null): Promise<string | null> {
    try {
      const result = await this.gateway.request<{ target: string | null }>("origin", { requesters, thread });
      return result.target;
    } catch (error) {
      this.diagnostics.add({
        source: "gateway",
        level: "error",
        message: "无法取得系统回合的回复目标，这一轮的最终回复不会发送",
        detail: (error as Error).message,
      });
      return null;
    }
  }

  private async handleReady(result: ReadyResult): Promise<void> {
    if (result.lastExit) {
      const exit = result.lastExit;
      this.diagnostics.add({
        source: "last-exit",
        level: "error",
        message: `上一个 agent 进程在 ${exit.at} 意外退出（退出码 ${exit.code ?? "无"}，信号 ${exit.signal ?? "无"}）`,
        detail: `stderr 最后 50 行：\n${exit.stderrTail}`,
      });
    }
    if (result.classifierRestore) {
      const restore = result.classifierRestore;
      if (restore.restored || restore.detail !== "") {
        this.diagnostics.add({
          source: "classifier-restore",
          level: "error",
          message: restore.restored
            ? "上一个进程退出时 daemon 恢复了被修改的 CLASSIFIER.md"
            : "上一个进程退出时 CLASSIFIER.md 有变化：回合开始时它不是普通文件，daemon 没有恢复，只报告了这次变化",
          detail: restore.detail,
          notifyAdmins: false,
        });
      }
    }
    if (result.applied) {
      const applied = result.applied;
      const origin = await this.origin(applied.origin.requester === null ? null : [applied.origin.requester], applied.origin.thread);
      this.dispatcher.system(this.appliedTurn(applied, origin));
    }
    if (result.incident) await this.handleIncident(result.incident);
    if (result.rollback) {
      const record = result.rollback;
      this.rollback.set(record);
      if (!record.notified) {
        const origin = await this.origin(record.requesters, record.thread);
        this.dispatcher.system({
          kind: "rollback",
          origin,
          attrs: [["rollback-id", String(record.id)]],
          body: () => rollbackTurnBody(record),
        });
      }
    }
  }

  private appliedTurn(applied: NonNullable<ReadyResult["applied"]>, origin: string | null): SystemTurnSpec {
    return {
      kind: "applied",
      origin,
      attrs: [["snapshot", applied.snapshot], ["requester", applied.origin.requester]],
      body: () => {
        const pi = this.activePi;
        const tools = pi ? pi.getAllTools().map((tool) => tool.name).join("、") : "（无法取得）";
        const commands = pi ? pi.getCommands().map((command) => command.name).join("、") : "（无法取得）";
        const middleware = this.middleware.list().map((entry) => `${entry.owner.name}（order ${entry.order}）`).join("、") || "无";
        return [
          `改动已生效（快照 ${applied.snapshot}）。摘要：${applied.summary}`,
          `发起者：${applied.origin.requester ?? "无"}，发起的会话串：${applied.origin.thread ?? "无"}`,
          `当前已加载的工具：${tools}  命令：${commands || "无"}  中间件：${middleware}`,
          "请验证新功能是否按预期工作，并把结果告诉发起的用户。",
        ];
      },
    };
  }

  private async handleIncident(incident: IncidentNotice): Promise<void> {
    this.lastIncident = incident;
    const origin = await this.origin(null, incident.origin.thread);
    this.dispatcher.system({
      kind: "incident",
      origin,
      attrs: [["incident", incident.id]],
      body: () => [
        `故障 ${incident.id} 已经由 recovery 修复，修复后的版本度过了观察期。`,
        `触发：${incident.trigger}`,
        `recovery 的修复说明：${incident.summary}`,
        `改动的文件：${incident.changedFiles.length === 0 ? "无" : incident.changedFiles.join("、")}`,
        `recovery 写的经验：${incident.lesson}`,
        `引起故障的应用：发起者 ${incident.origin.requester ?? "无"}，会话串 ${incident.origin.thread ?? "无"}`,
        "请完成以下事情：",
        "1. 阅读上面的修复说明；",
        "2. 确认 recovery 的修改没有去掉用户需要的功能，必要时按检查与应用流程重新实现；",
        "3. 把经验写入 AGENTS.md 的“经验记录”；",
        "4. 向用户说明发生了什么。",
      ],
    });
  }

  private onBridgeEvent(event: BridgeEvent): void {
    switch (event.type) {
      case "extension_error":
        // A rejected dispatch is reported once, as the error of the turn it ended.
        if (event.extensionPath === "<runtime>" && event.event === "send_user_message" && this.dispatcher.onDispatchRejected(event.error)) return;
        this.diagnostics.add({
          source: `extension_error:${event.extensionPath}:${event.event}`,
          level: "error",
          message: `扩展 ${event.extensionPath} 在 ${event.event} 中出错`,
          detail: event.error,
        });
        return;
      case "state":
        this.daemonState = event.state;
        this.daemonReason = event.reason;
        this.dispatcher.kick();
        return;
      case "incident":
        this.handleIncident(event.incident).then(undefined, (error: unknown) => this.fatal("处理故障通知", error));
        return;
      default:
        throw new Error(`daemon 发来未知的事件：${JSON.stringify(event)}`);
    }
  }

  private notifyAdmins(record: DiagnosticRecord): void {
    const text = formatAdminNotice(record);
    const run = async () => {
      const targets = new Set<string>();
      for (const admin of this.admins) {
        const result = await this.gateway.request<{ target: string | null }>("origin", { requesters: [admin], thread: null });
        if (result.target !== null) targets.add(result.target);
      }
      for (const target of targets) await this.gateway.post({ target, markdown: text });
    };
    run().then(undefined, (error: Error) => {
      this.diagnostics.add({ source: "admin-notify", level: "warning", message: "错误诊断没有发给管理员", detail: error.message, notifyAdmins: false });
    });
  }

  private postReply(reply: OutboundReply, card?: unknown): Promise<{ ref: string }> {
    const params: PostParams = { target: reply.target };
    if (reply.markdown !== "") params.markdown = reply.markdown;
    if (reply.files.length > 0) params.files = reply.files.map((file) => ({ path: resolve(this.config.workspaceDir, file) }));
    if (card !== undefined) params.card = card;
    if (reply.replyTo !== undefined) params.replyTo = reply.replyTo;
    return this.gateway.post(params);
  }

  // SDK API (DESIGN 8).

  connectExtension(generation: number, data: unknown): void {
    const request = data as Partial<BearbotConnectRequest> | null;
    if (!request || typeof request !== "object" || typeof request.resolve !== "function" || typeof request.name !== "string" || request.name === "") {
      this.diagnostics.add({ source: "sdk", level: "warning", message: "收到格式不正确的 bearbot:connect 请求" });
      return;
    }
    if (request.apiVersion !== 1) {
      this.diagnostics.add({ source: `extension:${request.name}`, level: "error", message: `扩展 ${request.name} 请求的 API 版本 ${String(request.apiVersion)} 不受支持（支持 1）` });
      return;
    }
    request.resolve(this.createApi(generation, request.name));
  }

  private createApi(generation: number, name: string): BearbotApi {
    const owner: Owner = { name, generation };
    const trial = this.config.trial;
    const live = () => {
      if (this.staleGenerations.has(generation)) {
        throw new Error(`扩展 ${name} 使用的 bearbot API 已经失效（会话已替换）：请在 session_shutdown 中停止定时器与其他后台工作`);
      }
    };
    const order = (options: { order: number } | undefined): number => {
      if (!options || typeof options.order !== "number") throw new Error("需要 options.order（数字）");
      return options.order;
    };
    const workspace = Object.freeze({ dir: this.config.workspaceDir });
    return {
      apiVersion: 1,
      workspace,
      trial,
      onMessage: (listener) => {
        live();
        return this.subscribers.add(listener, 0, owner);
      },
      onTurnStart: (listener) => {
        live();
        return this.turnStartListeners.add(listener, 0, owner);
      },
      onTurnEnd: (listener) => {
        live();
        return this.turnEndListeners.add(listener, 0, owner);
      },
      onBindingsChange: (listener) => {
        live();
        return this.bindingListeners.add(listener, 0, owner);
      },
      use: (middleware, options) => {
        live();
        return this.middleware.add(middleware, order(options), owner);
      },
      useOutbound: (middleware, options) => {
        live();
        return this.outboundMiddleware.add(middleware, order(options), owner);
      },
      prompt: async (request) => {
        live();
        assertNotTrial(trial, "prompt");
        await this.dispatcher.prompt(request, name);
      },
      post: async (target, message) => {
        live();
        assertNotTrial(trial, "post");
        const reply: OutboundReply = { target, markdown: message.markdown ?? "", files: message.files ?? [], turn: null };
        if (message.replyTo !== undefined) reply.replyTo = message.replyTo;
        const result = await this.outbound.run(reply, (current) => this.postReply(current, message.card)).result;
        if (result.kind === "dropped") throw new Error(`出站中间件 ${result.by} 丢弃了这条消息：${result.reason}`);
        return { ref: result.ref };
      },
      edit: async (ref, message) => {
        live();
        assertNotTrial(trial, "edit");
        await this.gateway.request("edit", { ref, ...message });
      },
      delete: async (ref) => {
        live();
        assertNotTrial(trial, "delete");
        await this.gateway.request("delete", { ref });
      },
      react: async (ref, emoji, remove) => {
        live();
        assertNotTrial(trial, "react");
        await this.gateway.request("react", { ref, emoji, ...(remove === undefined ? {} : { remove }) });
      },
      history: async (target, options) => {
        live();
        assertNotTrial(trial, "history");
        const result = await this.gateway.request<{ messages: WireMessage[] }>("history", {
          target,
          limit: options?.limit ?? 20,
          ...(options?.before === undefined ? {} : { before: options.before }),
        });
        return result.messages.map((wire): InboundMessage => ({ ...messageFromWire(wire), meta: { direction: wire.direction ?? "in" } }));
      },
      bindings: () => {
        live();
        assertNotTrial(trial, "bindings");
        return this.bindingList.map(toBinding);
      },
      isAdmin: (identity) => {
        live();
        assertNotTrial(trial, "isAdmin");
        return this.admins.includes(identity);
      },
      diagnostic: (entry) => {
        live();
        if (entry?.level !== "error" && entry?.level !== "warning") throw new Error("diagnostic 的 level 只能是 error 或 warning");
        if (typeof entry.message !== "string" || entry.message === "") throw new Error("diagnostic 需要 message");
        this.diagnostics.add({
          source: `extension:${name}`,
          level: entry.level,
          message: entry.message,
          ...(entry.detail === undefined ? {} : { detail: entry.detail }),
        });
      },
    };
  }

  // bearbot_status.

  async statusText(): Promise<string> {
    const status: StatusInfo = await this.agent.status();
    const pi = this.activePi;
    const lines: string[] = [];
    lines.push(`agent 状态：${status.state}${status.reason ? `（${status.reason}）` : ""}；PID ${status.pid ?? "无"}，启动于 ${status.startedAt ?? "无"}，重启 ${status.restarts} 次，时间窗口内意外退出 ${status.crashesInWindow} 次`);
    if (status.probationUntil) lines.push(`观察期到 ${status.probationUntil}`);
    lines.push(`会话 ${status.sessionId ?? "无"}；当前快照 ${status.runningSnapshot ?? "无"}；last-good ${status.lastGood ?? "无"}`);
    lines.push(`待应用：${status.pendingApply ? `${status.pendingApply.snapshot}（${status.pendingApply.summary}）` : "无"}`);
    lines.push(`当前故障：${status.incident ? `${status.incident.id}（${status.incident.trigger}，已尝试 ${status.incident.attempts} 次）` : "无"}`);
    if (this.lastIncident) lines.push(`最近修复的故障：${this.lastIncident.id}（${this.lastIncident.trigger}）：${this.lastIncident.summary}`);
    lines.push(`维护状态：${status.maintenance ? `${status.maintenance.reason}（自 ${status.maintenance.since}）` : "无"}`);
    lines.push(`daemon 推送的最近状态：${this.daemonState ?? "无"}${this.daemonReason ? `（${this.daemonReason}）` : ""}${this.restartPending ? "；已安排本轮结束后重启" : ""}`);
    const selfNames = Object.entries(this.self).map(([platform, user]) => `${platform}:${user}`).join("、");
    lines.push(`gateway：${this.gateway.connected ? "已连接" : "未连接"}${selfNames === "" ? "" : `；bot 用户名 ${selfNames}`}；已确认到 seq ${this.ack.acked ?? "无"}`);
    lines.push(`绑定的频道：${this.bindingList.map((binding) => `${binding.channel}（${binding.trigger}，${binding.replyMode}，${binding.interrupt}）`).join("、") || "无"}`);
    lines.push(`管理员：${this.admins.join("、") || "无"}`);
    const rules = this.classifierSettings.rules.map((rule) => `${rule.pattern} ${rule.action}`).join("、") || "无";
    lines.push(`classifier：${this.classifierSettings.enabled ? "开启" : "关闭"}，模型 ${this.config.classifierModel.provider}/${this.config.classifierModel.modelId}，管理员规则 ${rules}`);
    lines.push(`已加载的工具：${pi ? pi.getAllTools().map((tool) => tool.name).join("、") : "（运行时正在替换）"}`);
    lines.push(`命令：${pi ? pi.getCommands().map((command) => command.name).join("、") || "无" : "（运行时正在替换）"}`);
    const names = <T>(registry: Registry<T>, withOrder: boolean) =>
      registry.list().map((entry) => (withOrder ? `${entry.owner.name}（order ${entry.order}）` : entry.owner.name)).join("、") || "无";
    lines.push(`入站中间件：${names(this.middleware, true)}`);
    lines.push(`出站中间件：${names(this.outboundMiddleware, true)}`);
    lines.push(`订阅者：${names(this.subscribers, false)}`);
    lines.push(`onTurnStart：${names(this.turnStartListeners, false)}；onTurnEnd：${names(this.turnEndListeners, false)}；onBindingsChange：${names(this.bindingListeners, false)}`);
    const held = this.pipeline.held();
    lines.push(`处理中的入站消息：${held.length === 0 ? "无" : held.map((item) => `seq ${item.seq} 在 ${item.stage}（${Math.round((Date.now() - item.since) / 1000)} 秒）`).join("、")}`);
    const outboundHeld = this.outbound.held();
    lines.push(`出站中间件中的消息：${outboundHeld.length === 0 ? "无" : outboundHeld.map((item) => `${item.target} 在 ${item.middleware}`).join("、")}`);
    lines.push(...this.dispatcher.status());
    const drops = this.pipeline.drops;
    lines.push(`最近丢弃的消息：${drops.length === 0 ? "无" : ""}`);
    for (const drop of drops.slice(-10)) lines.push(`  seq ${drop.seq.join(",")}（${drop.thread}）被 ${drop.by} 丢弃：${drop.reason}`);
    const pending = this.diagnostics.pending();
    lines.push(`待处理的诊断：${pending.length} 条`);
    for (const record of pending.slice(-20)) lines.push(`  [${record.time}] ${record.level} ${record.source}：${record.message}`);
    const record = this.rollback.record;
    lines.push(record ? `尚未确认的回滚记录：\n${rollbackReminder(record)}` : "尚未确认的回滚记录：无");
    return lines.join("\n");
  }
}

// Kept on globalThis: ctx.reload() clears pi's factory cache and evaluates this module again, and the process must
// still have exactly one bridge (one gateway connection, one queue).
const INSTANCE_KEY = Symbol.for("bearbot-im.bridge");

/** The process-wide bridge; created by the first factory call of the process. */
export function getBridge(extensionDir: string): Bridge {
  const store = globalThis as unknown as Record<symbol, Bridge | undefined>;
  const existing = store[INSTANCE_KEY];
  if (existing) return existing;
  const bridge = new Bridge(readConfig(process.env, extensionDir));
  store[INSTANCE_KEY] = bridge;
  return bridge;
}
