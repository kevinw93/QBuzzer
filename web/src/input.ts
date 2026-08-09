/** Converts event.timeStamp to the epoch-based clock. Browsers measure it from
 * page load; some older engines (and jsdom) use epoch milliseconds. */
export function eventTime(timeStamp: number): number {
  return timeStamp > 1e12 ? timeStamp : performance.timeOrigin + timeStamp
}

export function isTyping(target: EventTarget | null): boolean {
  return target instanceof HTMLElement && (target.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName))
}
