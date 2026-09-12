export type UserMediaView = 'recent' | 'resume' | 'favorites';
export interface UserMediaItem {
  id: string; item_id: string; item_name: string; item_type: string; series_name: string;
  season_number: number | null; episode_number: number | null;
  runtime_ticks: number; position_ticks: number; played: boolean; is_favorite: boolean;
  last_played_at: string | null; updated_at: string; orphaned: boolean; source_ref?: string; metadata_status?: string;
}
export interface UserMediaPage {
  items: UserMediaItem[]; next_cursor: string; has_more: boolean; view: UserMediaView;
}
