export interface SubtitleTrack {
  index: number;
  language: string;
  name: string;
  state: string;
  format?: string;
  mode?: string;
  persistent?: boolean;
  bytes: number;
  cues: number;
  read_bytes: number;
  requests: number;
  reason?: string;
}

export interface SubtitleJob {
  id: string;
  item_id: string;
  source_id: string;
  source_name: string;
  source_size?: number;
  state: string;
  created_at: string;
  updated_at: string;
  viewers: number;
  tracks: SubtitleTrack[];
}

export interface SubtitlePage {
  enabled: boolean;
  reason?: string;
  boot_id: string;
  workers: number;
  active: number;
  cache_bytes: number;
  cache_budget: number;
  source_cache_bytes: number;
  source_cache_budget: number;
  source_read_bytes: number;
  source_hit_bytes: number;
  grants: number;
  jobs: SubtitleJob[];
  total_jobs: number;
  next_cursor: string;
}
