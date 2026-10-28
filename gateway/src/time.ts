function pad(n: number, width = 2): string {
  return String(n).padStart(width, "0");
}

/** ISO 8601 in the container's local time with offset, e.g. 2026-10-26T14:02:11+08:00. */
export function isoLocal(date: Date): string {
  const offsetMin = -date.getTimezoneOffset();
  const sign = offsetMin >= 0 ? "+" : "-";
  const abs = Math.abs(offsetMin);
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}` +
    `T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}` +
    `${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`
  );
}

/** HH:MM:SS in local time (console output). */
export function clock(date: Date): string {
  return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

/** YYYY-MM-DD in local time (inbox and console-files directories). */
export function localDate(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

/** YYYY-MM-DD HH:MM:SS in local time, for texts shown to users. */
export function localDateTime(date: Date): string {
  return `${localDate(date)} ${clock(date)}`;
}

/** Parses a YYYY-MM-DD directory name as the end of that local day; null for other names. */
export function endOfLocalDay(name: string): number | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(name);
  if (!m) {
    return null;
  }
  return new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]) + 1).getTime();
}
