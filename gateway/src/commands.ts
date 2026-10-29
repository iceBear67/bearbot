import { lstatSync } from "node:fs";
import { join } from "node:path";
import type { Adapter } from "chat";
import { parseArgsStringToArgv } from "string-argv";
import yargsParser from "yargs-parser";
import type { AgentState } from "./agent-state.ts";
import type { GatewayConfig } from "./config.ts";
import { errorText, type PinoLogger } from "./log.ts";
import type { Outbound } from "./outbound.ts";
import type { Platforms } from "./platforms.ts";
import { CONSOLE_CHANNEL, CONSOLE_OPERATOR, type BindingRow, type Repo } from "./repo.ts";
import { RpcError } from "./rpc.ts";
import { localDateTime } from "./time.ts";
import type { Interrupt, PlatformStatus, ReplyMode, StatusInfo, Trigger } from "./types.ts";

/** Requests from the gateway to bearbotd over gateway-ctl.sock (contracts 4.2). */
export interface DaemonClient {
  request(method: string, params: object): Promise<unknown>;
}

/** Notifications to bearbot-im after settings changed (DESIGN 6.7). */
export interface ChangeNotifier {
  bindingsChanged(): void;
  adminsChanged(): void;
  classifierChanged(): void;
}

export type CommandContext = {
  adapter: Adapter;
  platform: string;
  /** Thread the command came from; replies go there. */
  threadId: string;
  channelId: string;
  isDM: boolean;
  sender: { identity: string; name: string };
  /** Text after "/bb". */
  args: string;
  channelName: string | null;
  /** Author of the replied-to message (reply-based `/bb admin add`). */
  replyTarget: { identity: string; name: string | null } | null;
};

/** A user error; the message is the reply. */
export class CommandError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CommandError";
  }
}

const OPTION_KEYS = ["trigger", "prefix", "reply", "interrupt"] as const;
const TRIGGERS: readonly Trigger[] = ["mention", "all", "prefix"];
const REPLY_MODES: readonly ReplyMode[] = ["final", "each"];
const INTERRUPTS: readonly Interrupt[] = ["queue", "steer"];
const IDENTITY = /^(telegram|qq|wecom):\S+$/;

export const HELP_TEXT = [
  "bearbot 管理命令（只有管理员可以使用）：",
  "/bb help：命令列表",
  "/bb here：当前频道、会话串、绑定设置与 agent 状态",
  "/bb attach [--trigger mention|all|prefix] [--prefix <文本>] [--reply final|each] [--interrupt queue|steer]：绑定当前频道，私聊默认 all，群默认 mention；已绑定时按参数修改设置",
  "/bb detach：解除当前频道的绑定",
  "/bb new：开启新会话，所有绑定的频道都改用新会话",
  "/bb stop：中止正在进行的回合，队列中的消息不受影响",
  "/bb compact [说明]：压缩当前会话",
  "/bb restart：重启 agent 进程（等待回合结束）",
  "/bb status：组件状态、平台连接状态、绑定的频道、当前故障、维护状态",
  "/bb repair [说明]：维护状态下带着说明重新修复",
  "/bb resume：维护状态下不做改动直接恢复运行",
  "/bb rollback [快照]：回滚到指定快照，默认为最近一次应用之前的状态",
  "/bb snapshot list：快照列表",
  "/bb snapshot diff <引用> [引用]：两个快照之间的差异，只给一个引用时与它的父提交比较",
  "/bb admin list：管理员列表",
  "/bb admin add <身份>：添加管理员；也可以回复某人的消息发送 /bb admin add",
  "/bb admin remove <身份>：移除管理员",
  "/bb classifier status：classifier 开关、规则与 CLASSIFIER.md 的状态",
  "/bb classifier on|off：打开或关闭 classifier",
  "/bb classifier allow|deny <工具模式…>：增加放行或拒绝规则",
  "/bb classifier clear <工具模式…>：删除规则",
  "!bb <问题>：在不打断 agent 的情况下询问它正在做什么，由一个单独的会话根据当前会话的内容回答",
  "!steer <文本>：把这条消息插入自己正在进行的回合，不受绑定的 interrupt 设置限制；不能插入时按普通消息排队",
].join("\n");

type OptionKey = (typeof OPTION_KEYS)[number];

type Parsed = {
  positional: string[];
  options: Partial<Record<OptionKey, string>>;
  /** An option given twice; the subcommands decide whether that is an error. */
  repeated: OptionKey | null;
  /** An option given without a value ("/bb attach --prefix"); it is not in `options`. */
  missing: OptionKey | null;
};

const MISSING_VALUE = /^Not enough arguments following: (\S+)$/;

function parse(args: string): Parsed {
  const detailed = yargsParser.detailed(parseArgsStringToArgv(args), {
    string: [...OPTION_KEYS],
    // Each option takes the next word as its value, also when it starts with "-" ("--prefix -ai").
    narg: { trigger: 1, prefix: 1, reply: 1, interrupt: 1 },
    configuration: {
      "camel-case-expansion": false,
      "dot-notation": false,
      "parse-numbers": false,
      "parse-positional-numbers": false,
      "short-option-groups": false,
      // Unknown options stay positional, so a snapshot ref starting with "-" reaches the daemon's ref check.
      "unknown-options-as-args": true,
    },
  });
  let missing: OptionKey | null = null;
  if (detailed.error) {
    const m = MISSING_VALUE.exec(detailed.error.message);
    if (!m || !(OPTION_KEYS as readonly string[]).includes(m[1]!)) {
      throw detailed.error;
    }
    missing = m[1] as OptionKey;
  }
  const argv = detailed.argv;
  const options: Parsed["options"] = {};
  let repeated: OptionKey | null = null;
  for (const key of OPTION_KEYS) {
    const v: unknown = argv[key];
    if (v !== undefined) {
      if (Array.isArray(v)) {
        repeated ??= key;
      } else {
        options[key] = String(v);
      }
    }
  }
  return { positional: argv._.map(String), options, repeated, missing };
}

/** Text after the first `count` words of `args`, with its original spacing. */
function restAfter(args: string, count: number): string {
  let rest = args.trimStart();
  for (let i = 0; i < count; i++) {
    rest = rest.replace(/^\S+\s*/, "");
  }
  return rest.trim();
}

function oneOf<T extends string>(key: string, value: string, allowed: readonly T[]): T {
  if (!(allowed as readonly string[]).includes(value)) {
    throw new CommandError(`--${key} 的取值只能是 ${allowed.join("、")}`);
  }
  return value as T;
}

function describeBinding(b: BindingRow): string {
  const prefix = b.trigger === "prefix" ? `，prefix=${b.prefix}` : "";
  return `trigger=${b.trigger}${prefix}，reply=${b.replyMode}，interrupt=${b.interrupt}`;
}

function textResult(result: unknown): string {
  const text = (result as { text?: unknown } | null)?.text;
  if (typeof text !== "string") {
    throw new Error(`bearbotd 的响应中没有 text：${JSON.stringify(result)}`);
  }
  return text;
}

function time(iso: string | null): string {
  return iso === null ? "无" : localDateTime(new Date(iso));
}

export function formatPlatform(p: PlatformStatus): string {
  let line = `  ${p.name}：${p.state}`;
  if (p.since) line += `，自 ${time(p.since)}`;
  if (p.lastDisconnectAt) line += `，最近断开 ${time(p.lastDisconnectAt)}`;
  if (p.lastReconnectAt) line += `，最近重连 ${time(p.lastReconnectAt)}`;
  if (p.lastError) line += `，最近错误（${time(p.lastErrorAt)}）：${p.lastError}`;
  return line;
}

/** `/bb` commands (DESIGN 6.7). Only admins get an answer. */
export class Commands {
  constructor(
    private readonly deps: {
      cfg: GatewayConfig;
      repo: Repo;
      outbound: Outbound;
      platforms: Platforms;
      agentState: AgentState;
      daemon: DaemonClient;
      notifier: ChangeNotifier;
      logger: PinoLogger;
    },
  ) {}

  async run(ctx: CommandContext): Promise<void> {
    const { repo, outbound, logger } = this.deps;
    if (!repo.isAdmin(ctx.sender.identity)) {
      logger.info({ sender: ctx.sender.identity, thread: ctx.threadId }, "/bb from a non-admin ignored");
      return;
    }
    let reply: string;
    try {
      reply = await this.execute(ctx);
    } catch (e) {
      if (e instanceof CommandError || e instanceof RpcError) {
        reply = e.message;
      } else {
        logger.error({ err: e, args: ctx.args, thread: ctx.threadId }, "/bb command failed");
        reply = `命令执行失败：${errorText(e)}`;
      }
    }
    try {
      await outbound.post(ctx.threadId, { text: reply });
    } catch (e) {
      logger.error({ err: e, thread: ctx.threadId }, "sending the /bb reply failed");
    }
  }

  private async execute(ctx: CommandContext): Promise<string> {
    const p = parse(ctx.args);
    const [sub, ...rest] = p.positional;
    // compact and repair take a free-text note (restAfter): option words in it belong to the note.
    if (sub !== "compact" && sub !== "repair" && p.repeated !== null) {
      throw new CommandError(`参数 --${p.repeated} 只能给一次`);
    }
    const noOptions = (): void => {
      const given = Object.keys(p.options);
      if (given.length > 0) {
        throw new CommandError(`/bb ${sub} 不接受参数 --${given[0]}`);
      }
      if (p.missing !== null) {
        throw new CommandError(`/bb ${sub} 不接受参数 --${p.missing}`);
      }
    };
    const noExtra = (allowed: number): void => {
      if (rest.length > allowed) {
        throw new CommandError(`多余的参数：${rest.slice(allowed).join(" ")}`);
      }
    };
    switch (sub) {
      case undefined:
      case "help":
        return HELP_TEXT;
      case "here":
        noOptions();
        noExtra(0);
        return this.here(ctx);
      case "attach":
        noExtra(0);
        return this.attach(ctx, p.options, p.missing);
      case "detach":
        noOptions();
        noExtra(0);
        return this.detach(ctx);
      case "new":
        noOptions();
        noExtra(0);
        return textResult(await this.deps.daemon.request("session", { action: "new" }));
      case "stop":
        noOptions();
        noExtra(0);
        return this.stop();
      case "compact": {
        const instructions = restAfter(ctx.args, 1);
        return textResult(
          await this.deps.daemon.request("session", instructions === "" ? { action: "compact" } : { action: "compact", instructions }),
        );
      }
      case "restart":
        noOptions();
        noExtra(0);
        return textResult(await this.deps.daemon.request("restart", {}));
      case "status":
        noOptions();
        noExtra(0);
        return this.status();
      case "repair": {
        const note = restAfter(ctx.args, 1);
        return textResult(await this.deps.daemon.request("repair", note === "" ? {} : { note }));
      }
      case "resume":
        noOptions();
        noExtra(0);
        return textResult(await this.deps.daemon.request("resume", {}));
      case "rollback": {
        noOptions();
        noExtra(1);
        const params: Record<string, string> = { requester: ctx.sender.identity, thread: ctx.threadId };
        if (rest[0] !== undefined) params.snapshot = rest[0];
        return textResult(await this.deps.daemon.request("rollback", params));
      }
      case "snapshot":
        noOptions();
        return this.snapshot(rest);
      case "admin":
        noOptions();
        return this.admin(ctx, rest);
      case "classifier":
        noOptions();
        return this.classifier(ctx, rest);
      default:
        throw new CommandError(`未知的命令：/bb ${sub}，发送 /bb help 查看命令列表`);
    }
  }

  private here(ctx: CommandContext): string {
    const b = this.deps.repo.binding(ctx.channelId);
    const name = ctx.channelName ?? b?.channelName ?? null;
    const state = this.deps.agentState;
    return [
      `频道：${ctx.channelId}${name ? `（${name}）` : ""}`,
      `会话串：${ctx.threadId}`,
      `私聊：${ctx.isDM ? "是" : "否"}`,
      `绑定：${b ? describeBinding(b) : "未绑定"}`,
      `agent 状态：${state.state ?? "未知"}${state.reason ? `（${state.reason}）` : ""}`,
    ].join("\n");
  }

  private attach(ctx: CommandContext, options: Parsed["options"], missing: OptionKey | null): string {
    const { repo, notifier } = this.deps;
    if (missing !== null) {
      throw new CommandError(`参数 --${missing} 需要一个值`);
    }
    for (const [key, value] of Object.entries(options)) {
      if (value === "") {
        throw new CommandError(`参数 --${key} 需要一个值`);
      }
    }
    const existing = repo.binding(ctx.channelId);
    const trigger = options.trigger !== undefined ? oneOf("trigger", options.trigger, TRIGGERS) : (existing?.trigger ?? (ctx.isDM ? "all" : "mention"));
    const replyMode = options.reply !== undefined ? oneOf("reply", options.reply, REPLY_MODES) : (existing?.replyMode ?? "final");
    const interrupt =
      options.interrupt !== undefined ? oneOf("interrupt", options.interrupt, INTERRUPTS) : (existing?.interrupt ?? "queue");
    const prefix = options.prefix ?? existing?.prefix ?? null;
    if (trigger === "prefix" && (prefix === null || prefix === "")) {
      throw new CommandError("trigger 为 prefix 时需要用 --prefix 指定前缀");
    }
    repo.putBinding({
      channelId: ctx.channelId,
      platform: ctx.platform,
      channelName: ctx.channelName ?? existing?.channelName ?? null,
      trigger,
      prefix,
      replyMode,
      interrupt,
      createdBy: ctx.sender.identity,
    });
    notifier.bindingsChanged();
    const b = repo.binding(ctx.channelId)!;
    const name = b.channelName ? `（${b.channelName}）` : "";
    return `${existing ? "已更新绑定" : "已绑定"} ${b.channelId}${name}：${describeBinding(b)}`;
  }

  private detach(ctx: CommandContext): string {
    if (ctx.channelId === CONSOLE_CHANNEL) {
      throw new CommandError("控制台频道始终绑定");
    }
    if (!this.deps.repo.deleteBinding(ctx.channelId)) {
      throw new CommandError("当前频道没有绑定");
    }
    this.deps.notifier.bindingsChanged();
    return `已解除绑定 ${ctx.channelId}`;
  }

  private async stop(): Promise<string> {
    const turn = this.deps.agentState.turn;
    const text = textResult(await this.deps.daemon.request("session", { action: "abort" }));
    if (!turn.active) {
      return text;
    }
    if (turn.origin === null) {
      return `${text}\n该回合由扩展发起，没有回复目标（角色 ${turn.role}，开始于 ${time(turn.startedAt)}）`;
    }
    const channel = this.deps.platforms.adapterFor(turn.origin)?.channelIdFromThreadId(turn.origin) ?? turn.origin;
    const name = this.deps.repo.binding(channel)?.channelName;
    return `${text}\n该回合由 ${turn.origin}${name ? `（${name}）` : ""} 发起（角色 ${turn.role}，开始于 ${time(turn.startedAt)}）`;
  }

  private async status(): Promise<string> {
    const { repo, platforms, agentState, daemon } = this.deps;
    const lines: string[] = [];
    let info: StatusInfo | null = null;
    try {
      info = (await daemon.request("status", {})) as StatusInfo;
    } catch (e) {
      lines.push(`无法取得 bearbotd 的状态：${errorText(e)}`);
    }
    if (info) {
      lines.push(`agent：${info.state}${info.reason ? `（${info.reason}）` : ""}`);
      if (info.pid !== null) {
        lines.push(`进程：pid ${info.pid}，启动于 ${time(info.startedAt)}，重启 ${info.restarts} 次，窗口内意外退出 ${info.crashesInWindow} 次`);
      } else {
        lines.push(`进程：没有运行，重启 ${info.restarts} 次，窗口内意外退出 ${info.crashesInWindow} 次`);
      }
      if (info.probationUntil) lines.push(`观察期：至 ${time(info.probationUntil)}`);
      lines.push(`会话：${info.sessionId ?? "无"}`);
      lines.push(`快照：当前 ${info.runningSnapshot ?? "无"}，last-good ${info.lastGood ?? "无"}`);
      if (info.pendingApply) lines.push(`待应用：${info.pendingApply.snapshot}：${info.pendingApply.summary}`);
      lines.push(
        info.incident
          ? `当前故障：${info.incident.id}，触发：${info.incident.trigger}，已尝试修复 ${info.incident.attempts} 次`
          : "当前故障：无",
      );
      lines.push(
        info.maintenance
          ? `维护状态：${info.maintenance.reason}${info.maintenance.incidentId ? `（故障 ${info.maintenance.incidentId}）` : ""}，开始于 ${time(info.maintenance.since)}`
          : "维护状态：否",
      );
      if (info.rollback) {
        lines.push(`回滚记录：#${info.rollback.id}，目标 ${info.rollback.target}，${info.rollback.notified ? "已通知 agent" : "尚未通知 agent"}`);
      }
      lines.push(`控制台：${info.console ? "启用" : "没有启用"}`);
    } else if (agentState.state !== null) {
      lines.push(`agent（gateway 最近收到的状态）：${agentState.state}${agentState.reason ? `（${agentState.reason}）` : ""}`);
    }
    const turn = agentState.turn;
    lines.push(turn.active ? `当前回合：${turn.origin ?? "扩展发起"}，角色 ${turn.role}，开始于 ${time(turn.startedAt)}` : "当前回合：无");
    lines.push("平台：");
    for (const p of platforms.statuses()) {
      lines.push(formatPlatform(p));
    }
    const bindings = repo.bindings();
    lines.push(`绑定的频道（${bindings.length}）：`);
    for (const b of bindings) {
      lines.push(`  ${b.channelId}${b.channelName ? `（${b.channelName}）` : ""}：${describeBinding(b)}`);
    }
    return lines.join("\n");
  }

  private async snapshot(rest: string[]): Promise<string> {
    const [action, ref, ref2, ...extra] = rest;
    if (action === "list") {
      if (rest.length > 1) throw new CommandError(`多余的参数：${rest.slice(1).join(" ")}`);
      return textResult(await this.deps.daemon.request("snapshot.list", {}));
    }
    if (action === "diff") {
      if (ref === undefined) throw new CommandError("用法：/bb snapshot diff <引用> [引用]");
      if (extra.length > 0) throw new CommandError(`多余的参数：${extra.join(" ")}`);
      return textResult(await this.deps.daemon.request("snapshot.diff", ref2 === undefined ? { ref } : { ref, ref2 }));
    }
    throw new CommandError("用法：/bb snapshot list 或 /bb snapshot diff <引用> [引用]");
  }

  private admin(ctx: CommandContext, rest: string[]): string {
    const { repo, notifier } = this.deps;
    const [action, target, ...extra] = rest;
    if (extra.length > 0) {
      throw new CommandError(`多余的参数：${extra.join(" ")}`);
    }
    switch (action) {
      case "list": {
        if (target !== undefined) throw new CommandError(`多余的参数：${target}`);
        const lines = ["管理员："];
        for (const a of repo.adminInfos()) {
          const name = a.displayName ? `（${a.displayName}）` : "";
          if (a.builtin) {
            lines.push(`  ${a.identity}${name}：内置，通知会话串 ${a.notifyThread}`);
          } else {
            lines.push(
              `  ${a.identity}${name}：通知会话串 ${a.notifyThread ?? "无"}，由 ${a.addedBy ?? "未知"} 添加于 ${time(a.addedAt)}`,
            );
          }
        }
        if (lines.length === 1) lines.push("  （无）");
        return lines.join("\n");
      }
      case "add": {
        let identity: string;
        let name: string | null = null;
        if (target !== undefined) {
          identity = target;
        } else if (ctx.replyTarget) {
          identity = ctx.replyTarget.identity;
          name = ctx.replyTarget.name;
        } else {
          throw new CommandError("用法：/bb admin add <身份>，或回复某人的消息发送 /bb admin add");
        }
        if (identity === CONSOLE_OPERATOR) {
          throw new CommandError("console:operator 是内置管理员，不需要添加");
        }
        if (!IDENTITY.test(identity)) {
          throw new CommandError(`身份格式不正确：${identity}，应为 <平台>:<用户 id>，例如 telegram:123456`);
        }
        if (!repo.addAdmin({ identity, displayName: name, addedBy: ctx.sender.identity })) {
          return `${identity} 已经是管理员`;
        }
        notifier.adminsChanged();
        return `已添加管理员 ${identity}${name ? `（${name}）` : ""}`;
      }
      case "remove": {
        if (target === undefined) throw new CommandError("用法：/bb admin remove <身份>");
        if (target === CONSOLE_OPERATOR) {
          throw new CommandError("内置管理员 console:operator 不能移除");
        }
        if (!repo.removeAdmin(target)) {
          throw new CommandError(`${target} 不是管理员`);
        }
        notifier.adminsChanged();
        return `已移除管理员 ${target}`;
      }
      default:
        throw new CommandError("用法：/bb admin list | add <身份> | remove <身份>");
    }
  }

  private classifier(ctx: CommandContext, rest: string[]): string {
    const { repo, notifier, cfg } = this.deps;
    const [action, ...patterns] = rest;
    switch (action) {
      case "status": {
        if (patterns.length > 0) throw new CommandError(`多余的参数：${patterns.join(" ")}`);
        const info = repo.classifier();
        const setting = repo.classifierSetting();
        const source = setting
          ? `数据库设置，${setting.updatedBy} 修改于 ${localDateTime(new Date(setting.updatedAt))}`
          : "配置文件默认值";
        const lines = [`classifier：${info.enabled ? "开启" : "关闭"}（${source}）`, "规则："];
        for (const r of info.rules) lines.push(`  ${r.action} ${r.pattern}`);
        if (info.rules.length === 0) lines.push("  （无）");
        lines.push(`CLASSIFIER.md：${this.rulesFile(cfg.workspaceDir)}`);
        return lines.join("\n");
      }
      case "on":
      case "off":
        if (patterns.length > 0) throw new CommandError(`多余的参数：${patterns.join(" ")}`);
        repo.setClassifierEnabled(action === "on", ctx.sender.identity);
        notifier.classifierChanged();
        return action === "on" ? "classifier 已开启" : "classifier 已关闭";
      case "allow":
      case "deny":
        if (patterns.length === 0) throw new CommandError(`用法：/bb classifier ${action} <工具模式…>`);
        repo.putRules(patterns, action, ctx.sender.identity);
        notifier.classifierChanged();
        return `已添加${action === "allow" ? "放行" : "拒绝"}规则：${patterns.join("、")}`;
      case "clear": {
        if (patterns.length === 0) throw new CommandError("用法：/bb classifier clear <工具模式…>");
        const { removed, missing } = repo.clearRules(patterns);
        if (removed.length > 0) notifier.classifierChanged();
        const parts: string[] = [];
        if (removed.length > 0) parts.push(`已删除规则：${removed.join("、")}`);
        if (missing.length > 0) parts.push(`没有这些规则：${missing.join("、")}`);
        return parts.join("\n");
      }
      default:
        throw new CommandError("用法：/bb classifier status | on | off | allow <工具模式…> | deny <工具模式…> | clear <工具模式…>");
    }
  }

  private rulesFile(workspaceDir: string): string {
    let st;
    try {
      st = lstatSync(join(workspaceDir, "CLASSIFIER.md"));
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code === "ENOENT") {
        return "不存在";
      }
      throw e;
    }
    const kind = st.isFile() ? "普通文件" : st.isSymbolicLink() ? "符号链接" : st.isDirectory() ? "目录" : "其他类型的文件";
    return `存在（${kind}），${st.size} 字节，修改时间 ${localDateTime(st.mtime)}`;
  }
}
