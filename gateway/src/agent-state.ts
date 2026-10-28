import type { Repo } from "./repo.ts";
import { MAINTENANCE_STATES, type AgentWireState, type TurnInfo } from "./types.ts";

export type StateChange = {
  previous: AgentWireState | null;
  state: AgentWireState;
  reason: string | null;
};

/** The agent state last reported by bearbotd (`agent.state`) and the turn reported by bearbot-im (`turn`). */
export class AgentState {
  private current: AgentWireState | null = null;
  private currentReason: string | null = null;
  private turnInfo: TurnInfo = { active: false };
  private readonly listeners: ((change: StateChange) => void)[] = [];

  /**
   * `repo` keeps the channels that already got the maintenance notice in the current maintenance period
   * (DESIGN 5.8 step 4) in gateway.db, so a gateway restarted during maintenance does not notify them again.
   */
  constructor(private readonly repo: Repo) {}

  get state(): AgentWireState | null {
    return this.current;
  }

  get reason(): string | null {
    return this.currentReason;
  }

  get turn(): TurnInfo {
    return this.turnInfo;
  }

  inMaintenance(): boolean {
    return this.current !== null && MAINTENANCE_STATES.has(this.current);
  }

  onChange(listener: (change: StateChange) => void): void {
    this.listeners.push(listener);
  }

  /** Returns false when neither state nor reason changed. */
  update(state: AgentWireState, reason: string | null): boolean {
    if (state === this.current && reason === this.currentReason) {
      return false;
    }
    const previous = this.current;
    this.current = state;
    this.currentReason = reason;
    if (!MAINTENANCE_STATES.has(state)) {
      this.repo.clearMaintenanceNotices();
    }
    for (const l of this.listeners) {
      l({ previous, state, reason });
    }
    return true;
  }

  setTurn(turn: TurnInfo): void {
    this.turnInfo = turn;
  }

  /** True once per channel per maintenance period, only while in a maintenance state. */
  takeNotice(channelId: string): boolean {
    return this.inMaintenance() && this.repo.takeMaintenanceNotice(channelId);
  }
}
