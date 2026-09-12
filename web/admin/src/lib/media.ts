import { writable } from 'svelte/store';
import { apiRequest, session } from './api';
import type { MediaItem, MediaLocalState } from './types';

export interface MediaReference { itemId: string; sourceRef?: string | null; fallbackName?: string | null; at?: string | null; userId?: string | null; localState?: MediaLocalState | null }
interface Entry { item: MediaItem; expires: number }
const cache = new Map<string, Entry>();
const waiting = new Map<string, { ref: MediaReference; users: number }>();
const pending = new Set<string>();
export const mediaCache = writable<Map<string, Entry>>(new Map());
export const mediaSelection = writable<MediaReference | null>(null);
let flushTimer: ReturnType<typeof setTimeout> | undefined;
let inFlight = 0;
let generation = 0;
const controllers = new Set<AbortController>();
let trigger: HTMLElement | null = null;
let previousHash: string | null = null;
export function restoreLocalMedia(ref: MediaReference) {
  mediaSelection.update(current => current?.itemId === ref.itemId && current.userId === ref.userId && !current.localState && ref.localState ? { ...current, localState: ref.localState, fallbackName: current.fallbackName || ref.fallbackName } : current);
}
export const mediaKey = (id: string, source?: string | null) => `${source || ''}:${id}`;
export function mediaStatus(item?: MediaItem | null): string {
  if (!item) return 'Loading media…';
  if (item.stale) return 'Cached metadata · refresh unavailable';
  return ({ available: '', missing: 'Media no longer exists', unavailable: 'Metadata temporarily unavailable', source_changed: 'Media source cannot be verified' })[item.status];
}
export function mediaSubtitle(item?: MediaItem | null): string {
  if (!item) return '';
  const episode = `${item.season_number != null ? `S${String(item.season_number).padStart(2, '0')}` : ''}${item.episode_number != null ? `E${String(item.episode_number).padStart(2, '0')}` : ''}`;
  return [item.series_name, episode, item.production_year, item.runtime_ticks ? `${Math.round(item.runtime_ticks / 600_000_000)} min` : ''].filter(Boolean).join(' · ');
}
export function publishMedia(item: MediaItem, source?: string | null) {
  const key = mediaKey(item.id, source ?? item.source_ref);
  cache.delete(key);
  cache.set(key, { item, expires: Date.now() + (item.status === 'available' && !item.stale ? 600_000 : 15_000) });
  while (cache.size > 1000) cache.delete(cache.keys().next().value!);
  mediaCache.set(new Map(cache));
}
export function requestMedia(ref: MediaReference): () => void {
  const key = mediaKey(ref.itemId, ref.sourceRef);
  if (!ref.sourceRef) { publishMedia({ id: ref.itemId, source_ref: '', status: 'source_changed', name: ref.fallbackName || undefined }); return () => {}; }
  if ((cache.get(key)?.expires || 0) > Date.now() || pending.has(key)) return () => {};
  const entry = waiting.get(key) || { ref, users: 0 }; entry.users++; waiting.set(key, entry);
  schedule();
  return () => { const entry = waiting.get(key); if (entry && --entry.users <= 0) waiting.delete(key); };
}
function schedule() { if (!flushTimer) flushTimer = setTimeout(() => { flushTimer = undefined; void flush(); }, 35); }
async function flush() {
  if (inFlight >= 2 || waiting.size === 0) return;
  const source = waiting.values().next().value!.ref.sourceRef;
  const batch = [...waiting.entries()].filter(([, e]) => e.ref.sourceRef === source).slice(0, 50);
  batch.forEach(([key]) => { waiting.delete(key); pending.add(key); });
  inFlight++;
  const requestGeneration=generation; const controller=new AbortController(); controllers.add(controller);
  try {
    const query = new URLSearchParams({ ids: batch.map(([, e]) => e.ref.itemId).join(','), source_ref: source! });
    const response = await apiRequest<{ items: MediaItem[] }>(`/media/items?${query}`,{signal:controller.signal});
    if(requestGeneration!==generation || controller.signal.aborted) return;
    for (const [, entry] of batch) {
      const item = response.items?.find(item => item.id === entry.ref.itemId);
      publishMedia(item || { id: entry.ref.itemId, source_ref: source!, status: 'unavailable' }, source);
    }
  } catch {
    if(requestGeneration!==generation || controller.signal.aborted) return;
    for (const [key, entry] of batch) {
      const previous = cache.get(key)?.item;
      publishMedia(previous?.status === 'available' ? { ...previous, stale: true } : { id: entry.ref.itemId, source_ref: source!, status: 'unavailable' }, source);
    }
  } finally { if(requestGeneration===generation) batch.forEach(([key]) => pending.delete(key)); controllers.delete(controller); inFlight--; schedule(); }
  if (waiting.size) schedule();
}
export function openMedia(ref: MediaReference, element?: HTMLElement) {
  trigger = element || (document.activeElement as HTMLElement);
  previousHash = window.location.hash || '#/';
  mediaSelection.set(ref);
  const [route, query] = previousHash.split('?');
  const params = new URLSearchParams(query);
  params.set('media', ref.itemId);
  if (ref.sourceRef) params.set('media_source', ref.sourceRef); else params.delete('media_source');
  if (ref.at) params.set('media_at', ref.at); else params.delete('media_at');
  if (ref.userId) params.set('media_user', ref.userId); else params.delete('media_user');
  window.location.hash = `${route}?${params}`;
}
export function closeMedia() {
  if (previousHash) { const previous = previousHash; previousHash = null; window.history.back(); if (previous === window.location.hash) syncMediaHash(); }
  else {
    const [route, query] = window.location.hash.split('?');
    const params = new URLSearchParams(query);
    ['media', 'media_source', 'media_at', 'media_user'].forEach(key => params.delete(key));
    window.history.replaceState(null, '', `${route || '#/'}${params.size ? `?${params}` : ''}`);
    mediaSelection.set(null);
  }
  trigger?.focus(); trigger = null;
}
export function syncMediaHash() {
  const params = new URLSearchParams(window.location.hash.split('?')[1]);
  const id = params.get('media');
  if (!id) { mediaSelection.set(null); previousHash = null; return; }
  mediaSelection.update(old => old?.itemId === id && old.sourceRef === params.get('media_source') ? old : ({ itemId: id, sourceRef: params.get('media_source'), at: params.get('media_at'), userId: params.get('media_user') }));
}

// Session loss invalidates all metadata work, including responses already in flight.
session.subscribe(value => {
  if(value) return;
  generation++; controllers.forEach(controller=>controller.abort()); controllers.clear();
  waiting.clear(); pending.clear(); cache.clear();
  if(flushTimer) clearTimeout(flushTimer); flushTimer=undefined;
  mediaCache.set(new Map()); mediaSelection.set(null); trigger=null; previousHash=null;
});
