// Trial mode (DESIGN 7.9): BEARBOT_TRIAL=1 is set by the daemon for the trial start of a check. bearbot-im then
// connects to neither the gateway nor agent.sock and dispatches no turn; registration methods of the API keep
// working, methods that need the gateway throw, and diagnostics go to stderr (see Diagnostics with file null).

export const TRIAL_ERROR = "试运行中不能调用";

export class TrialError extends Error {
  constructor(method: string) {
    super(`${TRIAL_ERROR}（${method}）`);
  }
}

/** Throws in trial mode; used by every API method that needs the gateway or the dispatch queue. */
export function assertNotTrial(trial: boolean, method: string): void {
  if (trial) throw new TrialError(method);
}

export function parseTrialFlag(value: string | undefined): boolean {
  if (value === undefined || value === "") return false;
  if (value === "1") return true;
  throw new Error(`BEARBOT_TRIAL 只能是 1 或者不设置，当前为 ${value}`);
}
