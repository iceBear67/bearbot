import type { AssistantMessage, Message } from "@earendil-works/pi-ai";
import {
  buildSessionContext,
  createAgentSession,
  createExtensionRuntime,
  ModelRuntime,
  SessionManager,
  type AgentSession,
  type CreateAgentSessionOptions,
  type ExtensionContext,
  type ResourceLoader,
} from "@earendil-works/pi-coding-agent";
import { escapeAttr, escapeText } from "./render.ts";

// `!bb <问题>` (DESIGN 7.11): a separate pi session, started from the main session's context of this moment, answers
// a question about the work in progress without interrupting the main session. It follows `/btw:ask` of the pi-btw
// package: an in-memory child session seeded with the main session's messages, with pi's read-only tools only.

/** Calls in the btw session do not pass the classifier, so it gets pi's read-only built-in tools only. */
export const BTW_TOOLS = ["read", "grep", "find", "ls"];

export interface BtwRequest {
  question: string;
  thread: string;
  sender: { identity: string; name: string };
}

function nonEmptyString(value: unknown, name: string): string {
  if (typeof value !== "string" || value === "") throw new Error(`btw 请求的 ${name} 必须是非空字符串`);
  return value;
}

export function parseBtwRequest(params: unknown): BtwRequest {
  const p = (params ?? {}) as Record<string, unknown>;
  const sender = (p.sender ?? {}) as Record<string, unknown>;
  return {
    question: nonEmptyString(p.question, "question"),
    thread: nonEmptyString(p.thread, "thread"),
    sender: { identity: nonEmptyString(sender.identity, "sender.identity"), name: nonEmptyString(sender.name, "sender.name") },
  };
}

export function btwPromptText(request: BtwRequest): string {
  const from = `${request.sender.name} (${request.sender.identity})`;
  return `<bearbot-btw from="${escapeAttr(from)}">${escapeText(request.question)}</bearbot-btw>`;
}

/**
 * The main session's messages up to now: what is finished of the current turn is already in the session. System
 * messages also declare the main session's tools; without these declarations the btw session declares only its own.
 */
export function btwSeedMessages(ctx: ExtensionContext): Message[] {
  const messages = buildSessionContext(ctx.sessionManager.getEntries(), ctx.sessionManager.getLeafId()).messages as Message[];
  return messages.map((message) => {
    if (message.role !== "system") return message;
    const { toolsAdded: _added, toolsRemoved: _removed, ...rest } = message;
    return rest;
  });
}

/**
 * The child session's model runtime reads auth.json and models.json like the agent's; a provider registered by an
 * extension (pi.registerProvider) and a runtime API key exist only in the agent's runtime, so they are carried over.
 */
async function modelRuntimeOptions(ctx: ExtensionContext, provider: string): Promise<Pick<CreateAgentSessionOptions, "modelRuntime">> {
  const registry = ctx.modelRegistry;
  const nativeProvider = registry.getRegisteredNativeProvider(provider);
  const providerConfig = registry.getRegisteredProviderConfig(provider);
  const runtimeKey = registry.getProviderAuthStatus(provider).source === "runtime";
  if (!nativeProvider && !providerConfig && !runtimeKey) return {};
  const modelRuntime = await ModelRuntime.create({ allowModelNetwork: false });
  if (nativeProvider) modelRuntime.registerNativeProvider(nativeProvider);
  else if (providerConfig) modelRuntime.registerProvider(provider, providerConfig);
  await modelRuntime.refresh({ allowNetwork: false });
  if (runtimeKey) {
    const apiKey = await registry.getApiKeyForProvider(provider);
    if (apiKey === undefined) throw new Error(`服务 ${provider} 的运行时 API key 读取失败`);
    await modelRuntime.setRuntimeApiKey(provider, apiKey);
  }
  return { modelRuntime };
}

/** No extensions, skills, prompt templates or AGENTS files; the main session's current prompt plus prompt/btw.md. */
function resourceLoader(systemPrompt: string, instructions: string): ResourceLoader {
  const extensions = { extensions: [], errors: [], runtime: createExtensionRuntime() };
  return {
    getExtensions: () => extensions,
    getSkills: () => ({ skills: [], diagnostics: [] }),
    getPrompts: () => ({ prompts: [], diagnostics: [] }),
    getThemes: () => ({ themes: [], diagnostics: [] }),
    getAgentsFiles: () => ({ agentsFiles: [] }),
    getSystemPrompt: () => systemPrompt,
    getSystemPromptSource: () => undefined,
    getAppendSystemPrompt: () => [instructions],
    getAppendSystemPromptSources: () => [],
    extendResources: () => {},
    reload: async () => {},
  };
}

function answerText(message: AssistantMessage): string {
  return message.content
    .flatMap((block) => (block.type === "text" ? [block.text] : []))
    .join("\n")
    .trim();
}

export class Btw {
  private readonly running = new Set<AgentSession>();

  constructor(
    private readonly deps: {
      ctx: () => ExtensionContext | null;
      /** prompt/btw.md with its placeholders filled. */
      instructions: string;
    },
  ) {}

  async answer(request: BtwRequest): Promise<string> {
    const ctx = this.deps.ctx();
    if (!ctx) throw new Error("pi 的运行时当前不可用");
    const model = ctx.model;
    if (!model) throw new Error("agent 当前没有选择模型");
    const sessionManager = SessionManager.inMemory(ctx.cwd);
    for (const message of btwSeedMessages(ctx)) {
      sessionManager.appendMessage(message as Parameters<SessionManager["appendMessage"]>[0]);
    }
    const { session } = await createAgentSession({
      cwd: ctx.cwd,
      sessionManager,
      model,
      ...(ctx.thinkingLevel !== undefined ? { thinkingLevel: ctx.thinkingLevel } : {}),
      ...(await modelRuntimeOptions(ctx, model.provider)),
      tools: [...BTW_TOOLS],
      resourceLoader: resourceLoader(ctx.getSystemPrompt(), this.deps.instructions),
    });
    this.running.add(session);
    try {
      // A question starting with "/" is text, never a command or prompt template.
      await session.prompt(btwPromptText(request), { expandPromptTemplates: false });
      const last = session.state.messages.at(-1);
      if (last?.role !== "assistant") throw new Error("btw 会话结束时没有助手消息");
      const message = last as AssistantMessage;
      if (message.stopReason === "aborted") throw new Error("btw 会话被中止");
      if (message.stopReason === "error") throw new Error(message.errorMessage ?? "模型调用出错");
      const text = answerText(message);
      if (text === "") throw new Error("模型没有给出文字回答");
      return text;
    } finally {
      this.running.delete(session);
      session.dispose();
    }
  }

  /** The gateway connection closed: the answers could not be delivered any more. */
  abortAll(): void {
    for (const session of this.running) void session.abort();
  }
}
