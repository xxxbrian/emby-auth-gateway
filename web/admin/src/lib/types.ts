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

export interface UpstreamStatus {
  last_ok_at?: string;
  last_error_at?: string;
  last_status_class?: string;
  last_error_kind?: string;
  last_latency_ms?: number;
  auth_ok: boolean;
  last_auth_at?: string;
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
}

export interface SeriesData {
  rps: SeriesPoint[];
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
  latency_ms: number;
}

export interface ApiErrorBody {
  message?: string;
  error?: string;
  mfaId?: string;
}
