import type { AuditDTO } from './types';
export type AuditRecord = AuditDTO & {
  response_committed: boolean; is_error: boolean; severity: 'error' | 'warning' | 'info';
};
export interface AuditPage {
  items: AuditRecord[]; next_cursor: string; has_more: boolean; from: string; to: string;
}
