import { StringEnum } from "@earendil-works/pi-ai";
import type { AgentToolResult, ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import type { OutboundReply } from "../../../sdk/index.ts";
import type { AgentClient, CheckReport } from "./agent-client.ts";
import type { DiagnosticRecord } from "./diagnostics.ts";
import type { ActiveTurn } from "./dispatch.ts";
import type { GatewayClient, TargetsResult, WireMessage } from "./gateway-client.ts";
import type { OutboundResult } from "./pipeline.ts";
import { escapeAttr, messageFromWire, renderMessage } from "./render.ts";

// Tools of DESIGN 7.7.

export interface ToolsHost {
  turn(): ActiveTurn | null;
  gateway: GatewayClient;
  agent: AgentClient;
  sendOutbound(reply: OutboundReply): Promise<OutboundResult>;
  statusText(): Promise<string>;
  diagnosticsTail(n: number): DiagnosticRecord[];
  applyAccepted(snapshot: string): void;
}

function text(content: string, details: unknown = undefined): AgentToolResult<unknown> {
  return { content: [{ type: "text", text: content }], details };
}

export function formatCheckReport(report: CheckReport): string {
  const lines = [`检查 ${report.id}：${report.passed ? "通过" : "不通过"}`];
  const location = report.location;
  lines.push(`位置检查：${location.passed ? "通过" : "不通过"}${location.message === "" ? "" : `。${location.message}`}`);
  for (const violation of location.violations) lines.push(`  ${violation}`);
  const typecheck = report.typecheck;
  if (!typecheck.ran) lines.push("类型检查：没有运行");
  else if (typecheck.passed) lines.push("类型检查：通过");
  else {
    lines.push(`类型检查：不通过（${typecheck.errors.length} 个错误）`);
    for (const error of typecheck.errors) {
      const where = error.file === null ? "" : `${error.file}(${error.line ?? "?"},${error.column ?? "?"}): `;
      lines.push(`  ${where}${error.code} ${error.message}`);
    }
    if (typecheck.errors.length === 0 && typecheck.output !== "") lines.push(typecheck.output);
  }
  const trial = report.trial;
  if (!trial.ran) lines.push("试运行：没有运行");
  else {
    const exit = `退出码 ${trial.exitCode ?? "无"}，信号 ${trial.signal ?? "无"}${trial.timedOut ? "，超时" : ""}`;
    lines.push(`试运行：${trial.passed ? "通过" : `不通过（${exit}）`}`);
    for (const error of trial.extensionErrors) lines.push(`  extension_error ${error.extensionPath} ${error.event}：${error.error}`);
    for (const warning of trial.warnings) lines.push(`  警告：${warning}`);
    if (trial.stderr.trim() !== "") lines.push("  stderr：", trial.stderr.trimEnd());
  }
  lines.push(`报告文件：${report.reportPath}`);
  return lines.join("\n");
}

function formatHistory(target: string, messages: WireMessage[]): string {
  if (messages.length === 0) return `${target} 没有聊天记录`;
  const lines = [`<history target="${escapeAttr(target)}">`];
  for (const wire of messages) {
    lines.push(renderMessage(messageFromWire(wire), { seq: wire.seq.join(","), direction: wire.direction ?? "in" }));
  }
  lines.push("</history>");
  return lines.join("\n");
}

function formatTargets(result: TargetsResult): string {
  if (result.targets.length === 0) return "没有已绑定的频道";
  const lines = ["已绑定的频道："];
  for (const target of result.targets) {
    const name = target.name === null ? "" : `${target.name}，`;
    lines.push(`- ${target.channel}（${name}${target.platform}，${target.isDM ? "私聊" : "群"}）`);
    for (const thread of target.threads) lines.push(`  会话串 ${thread.thread}，最近活动 ${thread.lastActiveAt}`);
  }
  return lines.join("\n");
}

function requesterOf(turn: ActiveTurn): string | null {
  if (turn.kind !== "message") return null;
  return turn.messages.at(-1)?.sender.id ?? null;
}

export function registerTools(pi: ExtensionAPI, host: ToolsHost): void {
  pi.registerTool({
    name: "im_send",
    label: "IM 发送",
    description:
      "向聊天发送消息或文件。target 默认为本轮的回复目标（origin），也可以是其他已绑定的频道或会话串（用 im_targets 查看）。files 为工作区内的文件路径（相对工作区目录或绝对路径）。只在需要发送文件、发送到其他频道或长任务中途汇报时使用；本轮的最终回复会自动发回，不需要用它发送。",
    promptSnippet: "向聊天发送消息或文件（文件、其他频道、中途汇报）",
    parameters: Type.Object({
      text: Type.Optional(Type.String({ description: "markdown 文本" })),
      files: Type.Optional(Type.Array(Type.String(), { description: "要发送的文件路径" })),
      target: Type.Optional(Type.String({ description: "会话串 id 或频道 id，默认为本轮的 origin" })),
    }),
    async execute(_id, params) {
      const target = params.target ?? host.turn()?.origin ?? null;
      if (target === null) throw new Error("没有发送目标：本轮没有回复目标，请指定 target（可以用 im_targets 查看）");
      const files = params.files ?? [];
      if ((params.text ?? "").trim() === "" && files.length === 0) throw new Error("text 与 files 不能同时为空");
      const result = await host.sendOutbound({ target, markdown: params.text ?? "", files, turn: null });
      if (result.kind === "dropped") return text(`出站中间件 ${result.by} 丢弃了这条消息：${result.reason}`, result);
      return text(`已发送到 ${result.reply.target}（ref ${result.ref}）`, { ref: result.ref });
    },
  });

  pi.registerTool({
    name: "im_history",
    label: "IM 历史",
    description: "读取 gateway 记录的聊天历史（包括 bot 自己发出的消息，direction=\"out\"）。target 默认为本轮的 origin；before 是 seq，读取它之前的消息。",
    promptSnippet: "读取聊天历史",
    parameters: Type.Object({
      target: Type.Optional(Type.String({ description: "会话串 id 或频道 id，默认为本轮的 origin" })),
      limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 200, description: "条数，默认 20" })),
      before: Type.Optional(Type.String({ description: "只读取这个 seq 之前的消息" })),
    }),
    async execute(_id, params) {
      const target = params.target ?? host.turn()?.origin ?? null;
      if (target === null) throw new Error("没有目标：本轮没有回复目标，请指定 target");
      const result = await host.gateway.request<{ messages: WireMessage[] }>("history", {
        target,
        limit: params.limit ?? 20,
        ...(params.before === undefined ? {} : { before: params.before }),
      });
      return text(formatHistory(target, result.messages), { count: result.messages.length });
    },
  });

  pi.registerTool({
    name: "im_targets",
    label: "IM 目标",
    description: "列出已绑定的频道与它们最近活动的会话串。",
    promptSnippet: "列出已绑定的频道",
    parameters: Type.Object({}),
    async execute() {
      const result = await host.gateway.request<TargetsResult>("targets", {});
      return text(formatTargets(result), result);
    },
  });

  pi.registerTool({
    name: "bearbot_status",
    label: "bearbot 状态",
    description: "查看 bearbot 的状态：agent 状态、已加载的工具与命令、已注册的中间件与订阅者、待处理的诊断、最近的故障、尚未确认的回滚记录。",
    promptSnippet: "查看 bearbot 状态",
    parameters: Type.Object({}),
    async execute() {
      return text(await host.statusText());
    },
  });

  pi.registerTool({
    name: "bearbot_logs",
    label: "bearbot 日志",
    description: "读取日志：stderr（agent 进程的标准错误）、rpc（RPC 事件记录）、exits（进程退出记录）、diagnostics（诊断记录）。",
    promptSnippet: "读取 agent 日志与诊断",
    parameters: Type.Object({
      source: StringEnum(["stderr", "rpc", "exits", "diagnostics"] as const, { description: "日志来源" }),
      n: Type.Optional(Type.Integer({ minimum: 1, maximum: 2000, description: "最后多少行，默认 50" })),
    }),
    async execute(_id, params) {
      const n = params.n ?? 50;
      if (params.source === "diagnostics") {
        const records = host.diagnosticsTail(n);
        if (records.length === 0) return text("没有诊断记录");
        return text(records.map((record) => JSON.stringify(record)).join("\n"));
      }
      const lines = await host.agent.logs(params.source, n);
      return text(lines.length === 0 ? "（没有内容）" : lines.join("\n"));
    },
  });

  pi.registerTool({
    name: "bearbot_check",
    label: "bearbot 检查",
    description: "检查改动：位置检查、类型检查、试运行启动。修改扩展、技能、MCP 配置、设置之后先调用它，通过后再调用 bearbot_apply。",
    promptSnippet: "检查改动（位置、类型、试运行）",
    parameters: Type.Object({}),
    async execute() {
      const report = await host.agent.check();
      return text(formatCheckReport(report), report);
    },
  });

  pi.registerTool({
    name: "bearbot_apply",
    label: "bearbot 应用",
    description: "应用改动：daemon 重新检查，保存快照，在本轮结束后重启 agent 进程使改动生效。summary 写明改了什么、为什么改。",
    promptSnippet: "应用通过检查的改动（本轮结束后重启）",
    parameters: Type.Object({
      summary: Type.String({ description: "改了什么、为什么改" }),
    }),
    async execute(_id, params) {
      const turn = host.turn();
      if (!turn) throw new Error("没有进行中的回合");
      if (params.summary.trim() === "") throw new Error("summary 不能为空");
      const result = await host.agent.apply(params.summary, { thread: turn.origin, requester: requesterOf(turn) });
      if (!result.accepted) {
        return text(`检查没有通过，改动没有应用。\n${formatCheckReport(result.report)}`, result);
      }
      host.applyAccepted(result.snapshot);
      return text(`已安排，本轮结束后重启（快照 ${result.snapshot}）。请在本轮的最终回复中告诉用户即将重启。`, result);
    },
  });

  pi.registerTool({
    name: "bearbot_snapshot",
    label: "bearbot 快照",
    description: "快照操作：list 列出快照；diff 比较两个快照（只给 ref 时与它的父提交比较）；restore 把 paths 中的重启生效路径恢复到 ref，不填 paths 时恢复全部重启生效路径，恢复后需要再次检查与应用才会生效。",
    promptSnippet: "查看、比较、恢复快照",
    parameters: Type.Object({
      action: StringEnum(["list", "diff", "restore"] as const, { description: "操作" }),
      ref: Type.Optional(Type.String({ description: "快照引用，例如 apply/3、last-good" })),
      ref2: Type.Optional(Type.String({ description: "diff 的第二个引用" })),
      paths: Type.Optional(Type.Array(Type.String(), {
        description:
          "restore 时只恢复这些路径。路径相对快照工作目录 /bearbot（工作区的上一级目录），例如 workspace/.pi/extensions/foo.ts，不接受绝对路径；只能是重启生效路径（workspace/.pi/ 下的路径、profile/agent/settings.json、profile/agent/models.json）。不填时恢复全部重启生效路径",
      })),
    }),
    async execute(_id, params) {
      const result = await host.agent.snapshot({
        action: params.action,
        ...(params.ref === undefined ? {} : { ref: params.ref }),
        ...(params.ref2 === undefined ? {} : { ref2: params.ref2 }),
        ...(params.paths === undefined ? {} : { paths: params.paths }),
      });
      if (params.action === "restore") {
        const restored = result.restored ?? [];
        return text(`${result.text}\n恢复的文件：${restored.length === 0 ? "无" : restored.join("、")}\n恢复的改动需要再次调用 bearbot_check 与 bearbot_apply 才会生效。`, result);
      }
      return text(result.text, result);
    },
  });
}
