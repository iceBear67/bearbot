// Types of the gateway-ctl.sock and gateway.sock protocols (contracts sections 4 and 5).

export const AGENT_WIRE_STATES = [
  "stopped",
  "starting",
  "running",
  "draining",
  "restarting",
  "incident",
  "repairing",
  "maintenance",
  "rolling_back",
] as const;

export type AgentWireState = (typeof AGENT_WIRE_STATES)[number];

/** States in which triggering messages get the maintenance notice (DESIGN 5.8, 6.5). */
export const MAINTENANCE_STATES: ReadonlySet<AgentWireState> = new Set<AgentWireState>([
  "incident",
  "repairing",
  "rolling_back",
  "maintenance",
]);

export type StatusInfo = {
  state: AgentWireState;
  reason: string | null;
  pid: number | null;
  startedAt: string | null;
  restarts: number;
  crashesInWindow: number;
  probationUntil: string | null;
  sessionId: string | null;
  runningSnapshot: string | null;
  lastGood: string | null;
  pendingApply: { snapshot: string; summary: string } | null;
  incident: { id: string; trigger: string; attempts: number } | null;
  maintenance: { reason: string; incidentId: string | null; since: string } | null;
  rollback: { id: number; target: string; notified: boolean } | null;
  console: boolean;
};

export type PlatformState =
  | "connecting"
  | "connected"
  | "disconnected"
  | "error"
  | "interactive"
  | "line"
  | "input-ended";

export type PlatformStatus = {
  name: string;
  state: PlatformState;
  since: string | null;
  lastDisconnectAt: string | null;
  lastReconnectAt: string | null;
  lastError: string | null;
  lastErrorAt: string | null;
};

export type AdminInfo = {
  identity: string;
  displayName: string | null;
  notifyThread: string | null;
  addedBy: string | null;
  addedAt: string | null;
  builtin: boolean;
};

export type Trigger = "mention" | "all" | "prefix";
export type ReplyMode = "final" | "each";
export type Interrupt = "queue" | "steer";
export type MessageKind = "message" | "command" | "action" | "reaction";
export type AttachmentType = "image" | "file" | "audio" | "video";
export type Role = "admin" | "member";

export type WireBinding = {
  channel: string;
  name: string | null;
  platform: string;
  trigger: Trigger;
  prefix: string | null;
  replyMode: ReplyMode;
  interrupt: Interrupt;
};

export type WireAttachment = {
  type: AttachmentType;
  name: string;
  path?: string;
  size?: number;
  mimeType?: string;
  error?: string;
  data?: string;
};

export type WireMessage = {
  seq: number[];
  kind: MessageKind;
  id: string;
  platform: string;
  channel: { id: string; name?: string; isDM: boolean };
  thread: string;
  sender: { id: string; name: string; role: Role; isBot: boolean };
  time: string;
  text: string;
  mentionsBot: boolean;
  edited: boolean;
  stale: boolean;
  replyTo?: { id: string; sender: string; name: string; text: string };
  command?: { name: string; args: string };
  action?: { actionId: string; value?: string };
  reaction?: { emoji: string; added: boolean; messageId: string };
  attachments: WireAttachment[];
  trigger: boolean;
  direction?: "in" | "out";
};

export type ClassifierRuleInfo = { pattern: string; action: "allow" | "deny" };

export type ClassifierInfo = { enabled: boolean; rules: ClassifierRuleInfo[] };

export type TurnInfo =
  | { active: true; origin: string | null; role: "admin" | "member" | "system"; startedAt: string }
  | { active: false };

/** Stored in message_log.payload for inbound rows: the wire message without seq and stale. */
export type InboundPayload = Omit<WireMessage, "seq" | "stale" | "direction"> & {
  /** Attachment downloads are still running; the row is not delivered yet. */
  pending?: boolean;
};

/** Stored in message_log.payload for outbound rows. */
export type OutboundPayload = {
  platform: string;
  channel: { id: string; name?: string; isDM: boolean };
  text: string;
  attachments: WireAttachment[];
  card?: unknown;
};
