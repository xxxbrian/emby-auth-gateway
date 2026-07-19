/** API DTO contracts matching backend JSON from adminapi / adminquery / telemetry / adminauth. */

// --- Session (adminauth.Public + session create response) ---

export interface SessionPublic {
  csrf: string;
  email: string;
  superuser_id: string;
  expires_at: string;
  created?: string;
  last_seen?: string;
}

export interface ReauthResponse {
  reauth_ticket: string;
  expires_at: string;
  csrf: string;
  superuser_id: string;
  email: string;
}

// --- Telemetry Snapshot (nested) ---

/** Three-state backend auth observation from telemetry snapshot. */
export type UpstreamAuthState = 'unknown' | 'healthy' | 'failing';

export interface UpstreamStatus {
  last_ok_at?: string;
  last_error_at?: string;
  last_status_class?: string;
  last_error_kind?: string;
  last_latency_ms?: number;
  auth_state: UpstreamAuthState;
  last_auth_at?: string;
  /** Known: refresh_failed | auth_unavailable; tolerate unknown future strings. */
  last_auth_error?: string;
}

export interface CapacityStatus {
  active_playbacks: number;
  active_media_transfers: number;
  active_sessions: number;
  reject_rate_5m: number;
  rejects_5m: number;
}

export interface TrafficStatus {
  rps: number;
  mbps_in: number;
  mbps_out: number;
  error_rate_15m: number;
}

export interface ReliabilityStatus {
  userdata_write_fail_5m: number;
  overlay_fail_5m: number;
  telemetry_drops: number;
}

export interface RuntimeStatus {
  goroutines: number;
  heap_bytes: number;
}

export interface SeriesPoint {
  t: string;
  v: number;
  /** When true, this point represents a gap (no committed cycle). Charts break the line here. */
  gap?: boolean;
}

export interface SeriesData {
  window?: string; // 15m|1h|6h|24h
  interval?: string; // 1s|1m
  rps: SeriesPoint[];
  mbps_in?: SeriesPoint[];
  mbps_out: SeriesPoint[];
  errors: SeriesPoint[];
  playbacks: SeriesPoint[];
}

export interface Snapshot {
  ts: string;
  boot_id: string;
  uptime_sec: number;
  upstream: UpstreamStatus;
  capacity: CapacityStatus;
  traffic: TrafficStatus;
  reliability: ReliabilityStatus;
  runtime: RuntimeStatus;
  series?: SeriesData;
  media_buffer?: BufferAggregate;
}

// --- adminquery DTOs ---

export interface UserDTO {
  id: string;
  username: string;
  email?: string;
  synthetic_user_id: string;
  enabled: boolean;
  created?: string;
  updated?: string;
}

export interface SessionDTO {
  id: string;
  gateway_user_id: string;
  gateway_username?: string;
  synthetic_user_id?: string;
  client?: string;
  device?: string;
  device_id?: string;
  version?: string;
  remote_ip?: string;
  expires_at: string;
  revoked_at?: string;
  created?: string;
  active: boolean;
}

export interface AuditDTO {
  id: string;
  gateway_user_id?: string;
  synthetic_user_id?: string;
  event: string;
  message?: string;
  method?: string;
  path?: string;
  status?: number;
  remote_ip?: string;
  created: string;
  error_kind?: string;
  direction?: string;
  bytes_transferred?: number;
  duration_ms?: number;
  upstream_status?: number;
}

export interface UpstreamDTO {
  configured: boolean;
  key?: string;
  server_id?: string;
  server_name?: string;
  server_version?: string;
  version_checked_at?: string;
  backend_username?: string;
  password_set: boolean;
  token_set: boolean;
  backend_user_id?: string;
  token_updated_at?: string;
  last_login_at?: string;
  last_login_error?: string;
  backend_user_agent?: string;
  backend_authorization_client?: string;
  backend_authorization_device?: string;
  backend_authorization_version?: string;
  backend_authorization_device_id?: string;
  auth_generation_set?: boolean;
  base_url?: string;
  endpoint_key?: string;
  endpoint_active?: boolean;
}

// --- Activity (telemetry) ---

export interface Playback {
  session_id: string;
  user_id: string;
  username: string;
  device: string;
  item_id: string;
  item_name?: string;
  position_ticks: number;
  is_paused: boolean;
  last_seen: string;
  started_at: string;
}

export interface Transfer {
  session_id: string;
  user_id: string;
  username: string;
  device: string;
  item_id: string;
  media_mode: string;
  bytes_in: number;
  bytes_out: number;
  started_at: string;
  last_seen: string;
}

// --- Path policies (pathpolicy.Policy — Go encodes without json tags as PascalCase) ---

export interface Policy {
  ID?: string;
  Method?: string;
  Path?: string;
  Action?: string;
  Reason?: string;
  Priority?: number;
  Enabled?: boolean;
  Updated?: string;
  // tolerate lowercase if encoding changes
  id?: string;
  method?: string;
  path?: string;
  action?: string;
  reason?: string;
  priority?: number;
  enabled?: boolean;
  updated?: string;
}

export interface PolicyForm {
  id: string;
  method: string;
  path: string;
  action: string;
  reason: string;
  priority: number;
  enabled: boolean;
  /** Optimistic concurrency token from list (RFC3339). */
  updated?: string;
}

// --- Request bodies ---

export interface CreateUserBody {
  username: string;
  password: string;
  synthetic_user_id: string;
}

export interface PasswordBody {
  password: string;
}

export interface PolicyBody {
  method: string;
  path: string;
  action: string;
  reason: string;
  priority: number;
  enabled: boolean;
  /** Optional optimistic concurrency token (RFC3339). */
  updated?: string;
}

export interface UpstreamBody {
  emby_base_url: string;
  backend_username: string;
  backend_password: string;
  backend_user_agent: string;
  backend_authorization_client: string;
  backend_authorization_device: string;
  backend_authorization_version: string;
  force: boolean;
}

// --- Other API responses ---

export interface ItemsResponse<T> {
  items: T[];
}

export interface OkResponse {
  ok: boolean;
}

export interface RevokeResponse {
  revoked: number;
}

export interface InstallDefaultsResponse {
  created: number;
  preserved: number;
}

export interface SystemInfo {
  version: string;
  boot_id: string;
  started_at: string;
  uptime_sec: number;
  goroutines: number;
  heap_bytes: number;
  go_version: string;
}

export interface UpstreamProbeResult {
  server_id: string;
  server_name: string;
  server_version: string;
  backend_user_id?: string;
  latency_ms: number;
}

export interface PolicyPreviewResult {
  Allowed?: boolean;
  Action?: string;
  PolicyID?: string;
  Reason?: string;
  allowed?: boolean;
  action?: string;
  policy_id?: string;
  reason?: string;
}

export interface ApiErrorBody {
  message?: string;
  error?: string;
  mfaId?: string;
}

// --- Media Buffer Observability (ADR 0003) ---

/** Aggregate health enum for the buffer pool. */
export type BufferAggregateHealth = 'disabled' | 'idle' | 'healthy' | 'warning' | 'critical';

/** Per-stream health enum. */
export type BufferStreamHealth = 'healthy' | 'warning' | 'critical';

/** Observation completeness. */
export type ObservationCompleteness = 'complete' | 'limited' | 'unavailable';

/** Lifecycle state. */
export type BufferLifecycle = 'starting' | 'active' | 'closing';

/** Producer state. */
export type BufferProducerState = 'idle' | 'reading_base' | 'reading_optional' | 'waiting_for_buffer' | 'done';

/** Consumer state. */
export type BufferConsumerState = 'idle' | 'waiting_for_data' | 'writing' | 'done';

/** Allocation blocker. */
export type BufferAllocationBlocker = 'none' | 'pool_exhausted' | 'at_target' | 'debt';

/** Media mode. */
export type BufferMediaMode = 'direct' | 'hls' | 'range' | 'unknown';

/** Wait condition. */
export type BufferWaitCondition = 'none' | 'buffer_acquire' | 'pool_contention' | 'consumer_starvation' | 'upstream_stall' | 'downstream_stall' | 'close_join_stall';

/** Completion outcome. */
export type BufferOutcome = 'success' | 'canceled' | 'upstream_error' | 'downstream_error' | 'short_write' | 'length_mismatch' | 'invalid_read' | 'invalid_write' | 'no_progress' | 'invariant_error';

/** Aggregate media-buffer object from /overview or /media-buffer/series. */
export interface BufferAggregate {
  enabled: boolean;
  health: BufferAggregateHealth;
  health_reasons: string[];
  hard_budget_bytes: number;
  allocated_bytes: number;
  owned_bytes: number;
  free_bytes: number;
  unallocated_optional_bytes: number;
  private_base_bytes: number;
  queued_bytes: number;
  writing_bytes: number;
  active_requests: number;
  base_only_requests: number;
  indebted_requests: number;
  request_debt_bytes: number;
  buffer_acquire_count: number;
  pool_contention_count: number;
  consumer_starvation_count: number;
  upstream_stall_count: number;
  downstream_stall_count: number;
  close_join_stall_count: number;
  warning_streams: number;
  critical_streams: number;
  completion_drops: number;
  observed_active_requests: number;
  unobserved_active_requests: number;
  live_registration_drops: number;
  observation_completeness: ObservationCompleteness;
}

/** Live stream DTO from /media-buffer/streams or /media-buffer/streams/:id. */
export interface BufferStream {
  boot_id: string;
  stream_id: string;
  transfer_id: string | null;
  user_id: string | null;
  username: string | null;
  device: string | null;
  item_id: string | null;
  media_mode: BufferMediaMode;
  state: BufferLifecycle;
  producer_state: BufferProducerState;
  consumer_state: BufferConsumerState;
  allocation_blocker: BufferAllocationBlocker;
  target_bytes: number;
  owned_bytes: number;
  debt_bytes: number;
  private_base_bytes: number;
  queued_bytes: number;
  writing_bytes: number;
  bytes_read: number;
  bytes_written: number;
  wait_condition: BufferWaitCondition;
  wait_started_at: string | null;
  wait_duration_ms: number;
  health: BufferStreamHealth;
  health_reasons: string[];
  started_at: string;
  age_ms: number;
}

/** Paginated response for /media-buffer/streams. */
export interface BufferStreamsResponse {
  boot_id: string;
  items: BufferStream[];
  next_cursor: string | null;
  has_more: boolean;
  observation_completeness: ObservationCompleteness;
}

/** Stream detail response from /media-buffer/streams/:id — backend returns {boot_id, item}. */
export interface BufferStreamDetailResponse {
  boot_id: string;
  item: BufferStream | null;
}

/** Wait duration pair in completion summaries. */
export interface WaitDuration {
  total: number;
  max: number;
}

/** Completed stream summary from /media-buffer/recent. */
export interface BufferCompletion {
  boot_id: string;
  stream_id: string;
  transfer_id: string | null;
  user_id: string | null;
  username: string | null;
  device: string | null;
  item_id: string | null;
  media_mode: BufferMediaMode;
  final_state: 'closing';
  final_producer_state: BufferProducerState;
  final_consumer_state: BufferConsumerState;
  final_allocation_blocker: BufferAllocationBlocker;
  outcome: BufferOutcome;
  started_at: string;
  completed_at: string;
  duration_ms: number;
  bytes_read: number;
  bytes_written: number;
  peak_owned_bytes: number;
  peak_debt_bytes: number;
  peak_queued_bytes: number;
  peak_writing_bytes: number;
  waits_ms: {
    buffer_acquire: WaitDuration;
    pool_contention: WaitDuration;
    consumer_starvation: WaitDuration;
    upstream_stall: WaitDuration;
    downstream_stall: WaitDuration;
    close_join_stall: WaitDuration;
  };
  invariant_observed: boolean;
}

/** Recent completions response. */
export interface BufferRecentResponse {
  boot_id: string;
  items: BufferCompletion[];
}

/** Coherence domains descriptor for a series point. */
export interface BufferSeriesDomains {
  pool: 'coherent';
  sidecar: 'eventual';
}

/** Historical series point with presence. */
export interface BufferSeriesPoint {
  t: string;
  present: boolean;
  domains: BufferSeriesDomains | null;
  aggregate: BufferAggregate | null;
}

/** Series response from /media-buffer/series. */
export interface BufferSeriesResponse {
  boot_id: string;
  window: string;
  interval: string;
  points: BufferSeriesPoint[];
}

/** Transfer with optional buffer linkage for Activity integration. */
export interface TransferBufferLink {
  boot_id: string;
  stream_id: string;
}

/** Extended transfer DTO with buffer linkage. */
export interface TransferWithBuffer extends Transfer {
  media_buffer?: TransferBufferLink | null;
}
