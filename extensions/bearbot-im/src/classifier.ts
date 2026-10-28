import { realpathSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import type { Api, AssistantMessage, Context, Model, Tool, ToolCall } from "@earendil-works/pi-ai";
import { StringEnum } from "@earendil-works/pi-ai";
import type { ExtensionContext, ToolCallEvent, ToolCallEventResult, ToolInfo } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import type { InboundMessage, Role } from "../../../sdk/index.ts";
import type { AgentClient, ClassifierDecision, ClassifierTurnStart } from "./agent-client.ts";
import type { Diagnostics } from "./diagnostics.ts";
import type { ClassifierRule, ClassifierSettings } from "./gateway-client.ts";
import { escapeAttr, escapeText, localIso } from "./render.ts";

// Tool call check for member turns (DESIGN 7.10).

export function denyReason(role: Role, reason: string): string {
  const trimmed = reason.trim().replace(/[。.！!；;，,\s]+$/u, "");
  return `classifier 拒绝了这次调用（本轮角色 ${role}）：${trimmed}。需要执行时请管理员发起。`;
}

/** `*` matches any sequence of characters (also none); everything else matches literally; the whole name must match. */
export function matchPattern(pattern: string, name: string): boolean {
  const parts = pattern.split("*");
  if (parts.length === 1) return pattern === name;
  const first = parts[0];
  const last = parts[parts.length - 1];
  if (!name.startsWith(first)) return false;
  let position = first.length;
  for (let i = 1; i < parts.length - 1; i += 1) {
    const found = name.indexOf(parts[i], position);
    if (found === -1) return false;
    position = found + parts[i].length;
  }
  return name.length - last.length >= position && name.endsWith(last);
}

function literalLength(pattern: string): number {
  return pattern.replace(/\*/g, "").length;
}

/**
 * Admin rules from `/bb classifier allow|deny`. When several rules match, the most specific one decides: a pattern
 * without `*` first, then the pattern with the most literal characters; on a tie deny wins.
 */
export function adminRuleDecision(rules: readonly ClassifierRule[], toolName: string): ClassifierRule | null {
  const matching = rules.filter((rule) => matchPattern(rule.pattern, toolName));
  if (matching.length === 0) return null;
  const rank = (rule: ClassifierRule): [number, number, number] => [
    rule.pattern.includes("*") ? 0 : 1,
    literalLength(rule.pattern),
    rule.action === "deny" ? 1 : 0,
  ];
  matching.sort((a, b) => {
    const ra = rank(a);
    const rb = rank(b);
    return rb[0] - ra[0] || rb[1] - ra[1] || rb[2] - ra[2];
  });
  return matching[0];
}

const UNICODE_SPACES = new Set<number>([0x00a0, 0x202f, 0x205f, 0x3000]);
for (let code = 0x2000; code <= 0x200a; code += 1) UNICODE_SPACES.add(code);

/** Resolves a path argument the way pi's read/grep/find/ls tools do (unicode spaces, leading @, ~, file: URLs). */
export function resolveToolPath(raw: string, cwd: string): string {
  let path = "";
  for (const char of raw) path += UNICODE_SPACES.has(char.codePointAt(0) as number) ? " " : char;
  if (path.startsWith("@")) path = path.slice(1);
  if (path === "~") path = homedir();
  else if (path.startsWith("~/")) path = join(homedir(), path.slice(2));
  if (/^file:\/\//.test(path)) path = fileURLToPath(path);
  return isAbsolute(path) ? resolve(path) : resolve(cwd, path);
}

export type BuiltinResult = { kind: "allow" | "deny"; reason: string } | { kind: "model" };

const IM_TOOLS = new Set(["im_send", "im_history", "im_targets"]);
const PATH_TOOLS = new Set(["read", "grep", "find", "ls"]);

export function isInside(path: string, dir: string): boolean {
  return path === dir || path.startsWith(dir.endsWith(sep) ? dir : `${dir}${sep}`);
}

/** Whether the tool is pi's own tool of that name; an extension can register a tool under the same name. */
function isPiTool(tool: ToolInfo | undefined, path: string): boolean {
  return tool?.sourceInfo.path === path;
}

/**
 * Built-in rules of DESIGN 7.10. `workspaceReal` is the realpath of the workspace directory. `tool` is the
 * registered tool of the call: the rules for tool_search, read, grep, find, ls and codemode describe pi's own tools,
 * so an extension tool with one of these names goes to the model like every other extension tool.
 */
export function builtinDecision(
  toolName: string,
  input: Record<string, unknown>,
  cwd: string,
  workspaceReal: string,
  tool: ToolInfo | undefined,
): BuiltinResult {
  if (IM_TOOLS.has(toolName)) return { kind: "allow", reason: `内置规则放行 ${toolName}` };
  if (toolName === "tool_search" && isPiTool(tool, "builtin:tool-search")) return { kind: "allow", reason: `内置规则放行 ${toolName}` };
  if (PATH_TOOLS.has(toolName) && isPiTool(tool, `builtin:${toolName}`)) {
    const raw = typeof input.path === "string" && input.path !== "" ? input.path : ".";
    const resolved = resolveToolPath(raw, cwd);
    let real: string;
    try {
      real = realpathSync(resolved);
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code ?? "unknown";
      return { kind: "deny", reason: `路径 ${raw} 无法经 realpath 解析（${code}），无法确认它在工作区目录内` };
    }
    if (isInside(real, workspaceReal)) return { kind: "allow", reason: `内置规则放行：路径 ${real} 位于工作区目录内` };
    return { kind: "deny", reason: `路径 ${raw} 经 realpath 后为 ${real}，不在工作区目录 ${workspaceReal} 内` };
  }
  if (toolName === "write" || toolName === "edit" || toolName.startsWith("bearbot_")) {
    return { kind: "deny", reason: `内置规则在 member 回合中拒绝 ${toolName}（写入文件与 bearbot 管理操作只能由管理员发起）` };
  }
  if (toolName === "codemode" && isPiTool(tool, "builtin:codemode")) {
    return { kind: "allow", reason: "内置规则放行 codemode，脚本中的每个调用单独检查" };
  }
  return { kind: "model" };
}

/** Supplementary rules of the turn, from the daemon's turn start record. */
export type RulesState = { kind: "none" } | { kind: "rules"; content: string } | { kind: "deny"; reason: string };

export function rulesFromTurnStart(start: ClassifierTurnStart, maxBytes: number): RulesState {
  if (!start.exists) return { kind: "none" };
  if (!start.regular) return { kind: "deny", reason: "CLASSIFIER.md 不是普通文件（例如符号链接或目录），本轮交给模型判断的调用一律拒绝" };
  if (start.size > maxBytes) {
    return { kind: "deny", reason: `CLASSIFIER.md 大小为 ${start.size} 字节，超过上限 ${maxBytes} 字节，本轮交给模型判断的调用一律拒绝` };
  }
  if (start.content === undefined) throw new Error("daemon 的回合开始响应缺少 CLASSIFIER.md 的内容");
  return { kind: "rules", content: start.content };
}

/** JSON with object keys sorted, so equal arguments give equal cache keys. */
export function stableStringify(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value) ?? "null";
  if (Array.isArray(value)) return `[${value.map((item) => stableStringify(item)).join(",")}]`;
  const entries = Object.keys(value as Record<string, unknown>)
    .sort()
    .filter((key) => (value as Record<string, unknown>)[key] !== undefined)
    .map((key) => `${JSON.stringify(key)}:${stableStringify((value as Record<string, unknown>)[key])}`);
  return `{${entries.join(",")}}`;
}

/** toolChoice that forces the `decide` call; pi passes it to the provider unchanged, so it follows `model.api`. */
export function toolChoiceFor(api: Api): unknown {
  switch (api) {
    case "anthropic-messages":
    case "bedrock-converse-stream":
      return { type: "tool", name: DECIDE_TOOL_NAME };
    case "openai-completions":
    case "mistral-conversations":
    case "pi-messages":
      return { type: "function", function: { name: DECIDE_TOOL_NAME } };
    case "openai-responses":
    case "azure-openai-responses":
      return { type: "function", name: DECIDE_TOOL_NAME };
    case "openai-codex-responses":
      return "required";
    case "google-generative-ai":
    case "google-vertex":
      return "any";
    default:
      return null;
  }
}

export const DECIDE_TOOL_NAME = "decide";

const decideTool: Tool = {
  name: DECIDE_TOOL_NAME,
  description: "报告这次工具调用能否在 member 回合中执行。decision 为 allow（放行）或 deny（拒绝），reason 用简体中文写明理由。",
  parameters: Type.Object({
    decision: StringEnum(["allow", "deny"] as const, { description: "allow 放行，deny 拒绝" }),
    reason: Type.String({ description: "判断理由，简体中文" }),
  }),
};

export type Verdict = { kind: "decided"; decision: "allow" | "deny"; reason: string } | { kind: "failed"; error: string };

export interface TurnMessageForClassifier {
  from: string;
  role: string;
  text: string;
}

export function classifierMessages(messages: InboundMessage[]): TurnMessageForClassifier[] {
  return messages.map((message) => ({ from: `${message.sender.name} (${message.sender.id})`, role: message.sender.role, text: message.text }));
}

export function buildClassifierRequest(input: {
  workspace: string;
  messages: TurnMessageForClassifier[];
  toolName: string;
  tool: ToolInfo | undefined;
  args: Record<string, unknown>;
  nested: boolean;
}): string {
  const lines = [`<workspace>${escapeText(input.workspace)}</workspace>`, "<turn-messages>"];
  for (const message of input.messages) {
    lines.push(`<message from="${escapeAttr(message.from)}" sender-role="${escapeAttr(message.role)}">${escapeText(message.text)}</message>`);
  }
  lines.push("</turn-messages>");
  lines.push(`<tool-call name="${escapeAttr(input.toolName)}" nested="${input.nested ? "true" : "false"}">`);
  lines.push(`<description>${escapeText(input.tool?.description ?? "（没有找到这个工具的描述）")}</description>`);
  if (input.tool?.annotations) lines.push(`<annotations>${escapeText(JSON.stringify(input.tool.annotations))}</annotations>`);
  lines.push(`<arguments>${escapeText(JSON.stringify(input.args))}</arguments>`);
  lines.push("</tool-call>");
  lines.push("请调用 decide 给出判断。");
  return lines.join("\n");
}

/** The rules are escaped like every other text the classifier gets, so CLASSIFIER.md cannot close the tag. */
export function buildClassifierSystemPrompt(base: string, rules: string | null): string {
  if (rules === null) return base;
  return `${base}\n\n<workspace-rules>\n${escapeText(rules)}\n</workspace-rules>`;
}

/** One model judgment through ctx.modelRegistry with the single `decide` tool. */
export async function judgeWithModel(
  ctx: ExtensionContext,
  model: Model<Api>,
  systemPrompt: string,
  request: string,
  timeoutMs: number,
): Promise<Verdict> {
  const toolChoice = toolChoiceFor(model.api);
  if (toolChoice === null) return { kind: "failed", error: `classifier 模型的接口 ${model.api} 不支持强制调用 decide` };
  const context: Context = {
    systemPrompt,
    messages: [{ role: "user", content: [{ type: "text", text: request }], timestamp: Date.now() }],
    tools: [decideTool],
  };
  const timeout = AbortSignal.timeout(timeoutMs);
  const signal = ctx.signal ? AbortSignal.any([timeout, ctx.signal]) : timeout;
  const message: AssistantMessage = await ctx.modelRegistry.complete(model, context, { signal, maxTokens: 1024, toolChoice });
  if (timeout.aborted) return { kind: "failed", error: `classifier 超过 ${timeoutMs} 毫秒没有给出结果` };
  if (message.stopReason === "error" || message.stopReason === "aborted") {
    return { kind: "failed", error: message.errorMessage ?? `模型调用以 ${message.stopReason} 结束` };
  }
  const call = message.content.find((block): block is ToolCall => block.type === "toolCall" && block.name === DECIDE_TOOL_NAME);
  if (!call) return { kind: "failed", error: "模型没有调用 decide" };
  if (call.argumentsError) return { kind: "failed", error: `decide 的参数不合法：${call.argumentsError}` };
  const args = call.arguments as Record<string, unknown>;
  const decision = args.decision;
  const reason = args.reason;
  if ((decision !== "allow" && decision !== "deny") || typeof reason !== "string" || reason.trim() === "") {
    return { kind: "failed", error: `decide 的参数不合法：${JSON.stringify(args)}` };
  }
  return { kind: "decided", decision, reason };
}

/** Classifier state of one member turn. */
export interface ClassifierTurn {
  id: string;
  role: Role;
  rules: RulesState;
  rulesWarned: boolean;
  cache: Map<string, Promise<Verdict>>;
  /** Trigger messages and messages steered into the turn. */
  messages: () => TurnMessageForClassifier[];
}

export interface ClassifierDeps {
  settings: () => ClassifierSettings;
  turn: () => ClassifierTurn | null;
  toolInfo: (name: string) => ToolInfo | undefined;
  agent: AgentClient;
  diagnostics: Diagnostics;
  systemPrompt: string;
  model: { provider: string; modelId: string };
  timeoutMs: number;
  workspaceReal: string;
}

const ARGS_SUMMARY_LIMIT = 500;

export class Classifier {
  constructor(private readonly deps: ClassifierDeps) {}

  async onToolCall(event: ToolCallEvent, ctx: ExtensionContext): Promise<ToolCallEventResult | undefined> {
    const turn = this.deps.turn();
    if (!turn) {
      this.deps.diagnostics.add({
        source: "classifier",
        level: "warning",
        message: `没有进行中的 bearbot 回合，拒绝了工具调用 ${event.toolName}`,
      });
      return { block: true, reason: denyReason("member", "当前没有 bearbot 回合，无法确定本轮角色") };
    }
    if (turn.role !== "member") return undefined;
    if (!this.deps.settings().enabled) return undefined;

    const started = Date.now();
    const input = event.input as Record<string, unknown>;
    const record = (decision: "allow" | "deny", reason: string, basis: ClassifierDecision["basis"]) => {
      const entry: ClassifierDecision = {
        time: localIso(new Date()),
        turnId: turn.id,
        tool: event.toolName,
        argsSummary: summarize(input),
        decision,
        reason,
        basis,
        durationMs: Date.now() - started,
      };
      this.deps.agent.classifierDecision(entry).then(undefined, (error: Error) => {
        this.deps.diagnostics.add({ source: "classifier-record", level: "warning", message: "classifier 判断记录没有交给 daemon", detail: error.message });
      });
      return decision === "allow" ? undefined : { block: true, reason: denyReason(turn.role, reason) };
    };

    const rule = adminRuleDecision(this.deps.settings().rules, event.toolName);
    if (rule) {
      const verb = rule.action === "allow" ? "放行" : "拒绝";
      return record(rule.action, `管理员规则 ${rule.pattern} ${verb} ${event.toolName}`, "admin-rule");
    }

    const builtin = builtinDecision(event.toolName, input, ctx.cwd, this.deps.workspaceReal, this.deps.toolInfo(event.toolName));
    if (builtin.kind !== "model") return record(builtin.kind, builtin.reason, "builtin");

    if (turn.rules.kind === "deny") {
      if (!turn.rulesWarned) {
        turn.rulesWarned = true;
        this.deps.diagnostics.add({ source: "classifier-rules", level: "warning", message: turn.rules.reason });
      }
      return record("deny", turn.rules.reason, "rules-file");
    }

    const key = `${event.toolName}\u0000${stableStringify(input)}`;
    const cached = turn.cache.get(key);
    if (cached) {
      const verdict = await cached;
      if (verdict.kind === "decided") return record(verdict.decision, verdict.reason, "cache");
    }
    const pending = this.judge(event, ctx, turn, input);
    turn.cache.set(key, pending);
    const verdict = await pending;
    if (verdict.kind === "decided") return record(verdict.decision, verdict.reason, "model");

    turn.cache.delete(key);
    if (ctx.signal?.aborted) return record("deny", "回合已被中止", "classifier-error");
    this.deps.diagnostics.add({
      source: "classifier",
      level: "warning",
      message: `classifier 出错，拒绝了工具调用 ${event.toolName}`,
      detail: verdict.error,
    });
    return record("deny", `classifier 出错（${verdict.error}）`, "classifier-error");
  }

  private async judge(event: ToolCallEvent, ctx: ExtensionContext, turn: ClassifierTurn, input: Record<string, unknown>): Promise<Verdict> {
    const { provider, modelId } = this.deps.model;
    const model = ctx.modelRegistry.find(provider, modelId);
    if (!model) return { kind: "failed", error: `模型 ${provider}/${modelId} 不在模型列表中` };
    const request = buildClassifierRequest({
      workspace: this.deps.workspaceReal,
      messages: turn.messages(),
      toolName: event.toolName,
      tool: this.deps.toolInfo(event.toolName),
      args: input,
      nested: event.parentToolCallId !== undefined,
    });
    const systemPrompt = buildClassifierSystemPrompt(this.deps.systemPrompt, turn.rules.kind === "rules" ? turn.rules.content : null);
    return judgeWithModel(ctx, model, systemPrompt, request, this.deps.timeoutMs);
  }
}

function summarize(input: Record<string, unknown>): string {
  const text = JSON.stringify(input);
  return text.length > ARGS_SUMMARY_LIMIT ? `${text.slice(0, ARGS_SUMMARY_LIMIT)}…` : text;
}
