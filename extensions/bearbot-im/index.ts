import { dirname } from "node:path";
import { fileURLToPath } from "node:url";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { CONNECT_EVENT, getBridge } from "./src/bridge.ts";
import { registerTools } from "./src/tools.ts";

const extensionDir = dirname(fileURLToPath(import.meta.url));

/**
 * bearbot-im (DESIGN 7). Loaded first with `-e`. pi calls this factory again with a new `pi` after a runtime
 * replacement; the bridge keeps connections, queue and registrations at process level and rebinds to the newest pi.
 */
export default function bearbotIm(pi: ExtensionAPI): void {
  const bridge = getBridge(extensionDir);
  const generation = bridge.attach();

  pi.events.on(CONNECT_EVENT, (data) => bridge.connectExtension(generation, data));

  pi.on("project_trust", (event) => bridge.projectTrust(event.cwd));
  pi.on("resources_discover", () => ({ skillPaths: bridge.skillPaths }));
  pi.on("session_start", (_event, ctx) => bridge.onSessionStart(generation, pi, ctx));
  pi.on("session_shutdown", (event) => bridge.onSessionShutdown(generation, event));
  pi.on("input", (event) => bridge.onInput(event));
  pi.on("before_agent_start", (event, ctx) => bridge.onBeforeAgentStart(event, ctx));
  pi.on("agent_start", () => bridge.onAgentStart());
  pi.on("message_end", (event) => bridge.onMessageEnd(event));
  pi.on("agent_settled", (event) => bridge.onAgentSettled(event));
  pi.on("session_compact", (event) => bridge.onSessionCompact(event));
  pi.on("session_compact_failed", () => bridge.kick());
  pi.on("tool_call", (event, ctx) => bridge.onToolCall(event, ctx));

  registerTools(pi, bridge.toolsHost);
}
