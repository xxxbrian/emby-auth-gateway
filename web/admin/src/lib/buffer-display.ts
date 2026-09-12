import type { BufferStream } from './types';
export function bufferBytes(value: number | null | undefined): string {
  const v = Math.max(0, value || 0);
  return v >= 1024 ** 3 ? `${(v / 1024 ** 3).toFixed(1)} GiB` : v >= 1024 ** 2 ? `${(v / 1024 ** 2).toFixed(1)} MiB` : v >= 1024 ? `${(v / 1024).toFixed(1)} KiB` : `${v} B`;
}
export const conditionNames: Record<string, string> = { none: 'No active wait', buffer_acquire: 'Waiting for buffer capacity', pool_contention: 'Shared pool contention', consumer_starvation: 'Waiting for upstream data', upstream_stall: 'Upstream read stalled', downstream_stall: 'Client write stalled', close_join_stall: 'Stream close stalled' };
export function streamStatus(s: BufferStream): string {
  if (s.health_reasons?.length) return s.health_reasons.map(reason => conditionNames[reason] || reason.replaceAll('_', ' ')).join(' · ');
  if (s.state === 'closing') return 'Closing the stream';
  if (s.producer_state === 'waiting_for_buffer') {
    if (s.allocation_blocker === 'at_target') return 'At buffer limit · waiting for client to drain';
    if (s.allocation_blocker === 'debt') return 'Draining memory above the current allowance';
    if (s.allocation_blocker === 'pool_exhausted') return 'Waiting for shared pool capacity';
  }
  if (s.consumer_state === 'waiting_for_data') return 'Waiting for more upstream data';
  if (s.consumer_state === 'writing') return 'Sending data to the client';
  if (s.producer_state.startsWith('reading')) return 'Reading from upstream';
  return 'Stream ready';
}
