<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import { apiRequest } from './api';
  import { mediaSelection, mediaCache, mediaKey, mediaStatus, mediaSubtitle, publishMedia, closeMedia, syncMediaHash } from './media';
  import type { MediaItem } from './types';
  let dialog: HTMLDialogElement;
  let item = $state<MediaItem | null>(null);
  let loading = $state(false);
  let error = $state('');
  let imageFailed = $state(false);
  let copied = $state(false);
  let retry = $state(0);
  let closeButton = $state<HTMLButtonElement>();
  let image = $derived(item?.image?.replace(/([?&])size=small/, '$1size=large'));
  let local = $derived($mediaSelection?.localState);
  const clock = (ticks?: number) => { const s = Math.floor((ticks || 0) / 10_000_000); return `${Math.floor(s / 3600) ? `${Math.floor(s / 3600)}:` : ''}${String(Math.floor(s / 60) % 60).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`; };
  $effect(() => {
    const ref = $mediaSelection; void retry;
    if (!ref) { dialog?.close(); return; }
    const ctrl = new AbortController();
    imageFailed = false; copied = false; error = '';
    item = untrack(() => $mediaCache.get(mediaKey(ref.itemId, ref.sourceRef))?.item || null);
    tick().then(() => { if (!ctrl.signal.aborted && !dialog.open) { dialog.showModal(); closeButton?.focus(); } });
    if (!ref.sourceRef) { item = { id: ref.itemId, source_ref: '', name: ref.fallbackName || undefined, status: 'source_changed' }; loading = false; return () => ctrl.abort(); }
    loading = true;
    apiRequest<MediaItem>(`/media/items/${encodeURIComponent(ref.itemId)}?${new URLSearchParams({ source_ref: ref.sourceRef })}`, { signal: ctrl.signal }).then(result => {
      if (!ctrl.signal.aborted) { item = result; publishMedia(result, ref.sourceRef); loading = false; }
    }).catch(err => { if (!ctrl.signal.aborted) { error = err instanceof Error ? err.message : 'Unable to load media'; loading = false; } });
    return () => ctrl.abort();
  });
  onMount(() => { syncMediaHash(); window.addEventListener('hashchange', syncMediaHash); return () => { window.removeEventListener('hashchange', syncMediaHash); mediaSelection.set(null); }; });
  async function copyId() { try { await navigator.clipboard.writeText($mediaSelection!.itemId); copied = true; } catch { error = 'Copy unavailable. Select the item ID below to copy it.'; } }
</script>
<dialog bind:this={dialog} class="media-drawer" aria-labelledby="media-title" oncancel={(event) => { event.preventDefault(); closeMedia(); }} onclick={(event) => { if (event.target === dialog) { const rect = dialog.getBoundingClientRect(); if (event.clientX < rect.left) closeMedia(); } }}>
  {#if $mediaSelection}
    <div class="drawer-header"><h2 class="drawer-title" id="media-title">Media details</h2><button bind:this={closeButton} class="secondary" onclick={closeMedia} aria-label="Close media details">Close</button></div>
    <div class="drawer-body">
      <div class="hero">
        <div class="cover">{#if image && !imageFailed}<img src={image} alt={`Cover for ${item?.name || 'media'}`} onerror={() => imageFailed = true} />{:else}<span>NO IMAGE</span>{/if}</div>
        <div><span class="eyebrow">{item?.type || 'Media resource'}</span><h3>{item?.name || $mediaSelection.fallbackName || 'Media details'}</h3><p class="subtitle">{mediaSubtitle(item)}</p>{#if item?.community_rating != null}<span class="rating">★ {item.community_rating.toFixed(1)}</span>{/if}{#if item?.official_rating}<span class="rating">{item.official_rating}</span>{/if}</div>
      </div>
      {#if loading}<p class="text-secondary" role="status">Loading full metadata…</p>{/if}
      {#if error}<div class="error-message" role="alert">{error} <button class="secondary" onclick={() => retry++}>Retry</button></div>{/if}
      {#if item && mediaStatus(item)}<div class="notice" role="status">{mediaStatus(item)}.{#if item.status === 'source_changed'} This reference is not associated with the current media source. Existing identifiers and saved titles remain available.{:else if item.status === 'unavailable'} Playback monitoring remains available. <button class="secondary" onclick={() => retry++}>Retry</button>{/if}</div>{/if}
      {#if item?.overview}<section><h4>Overview</h4><p class="overview">{item.overview}</p></section>{:else if item?.status === 'available' && !loading}<p class="text-secondary text-sm">No overview was provided for this resource.</p>{/if}
      {#if item?.genres?.length}<div class="tags">{#each item.genres as genre}<span>{genre}</span>{/each}</div>{/if}
      {#if local}<section><h4>Gateway user state</h4><p class="text-secondary text-xs">Personal state for this Gateway user, reported when this view was opened.</p><div class="facts"><div><span>Playback position</span><strong>{clock(local.position_ticks)} / {local.runtime_ticks || item?.runtime_ticks ? clock(local.runtime_ticks || item?.runtime_ticks) : 'Duration unavailable'}</strong></div>{#if local.played != null}<div><span>Watched</span><strong>{local.played ? 'Yes' : 'No'}</strong></div>{/if}{#if local.is_favorite != null}<div><span>Favorite</span><strong>{local.is_favorite ? 'Yes' : 'No'}</strong></div>{/if}{#if local.last_played_at}<div><span>Last played</span><strong>{new Date(local.last_played_at).toLocaleString()}</strong></div>{/if}</div></section>{/if}
      {#if item?.container || item?.media_streams?.length}<section><h4>Media information</h4>{#if item.container}<p class="text-secondary text-sm">Container <strong class="mono">{item.container.toUpperCase()}</strong></p>{/if}<div class="tracks">{#each item.media_streams || [] as track}<div><strong>{track.type}</strong><span>{[track.codec?.toUpperCase(), track.width && track.height ? `${track.width} × ${track.height}` : '', track.channels ? `${track.channels} channels` : '', track.language, track.title, track.is_default ? 'Default' : '', track.is_external ? 'External' : ''].filter(Boolean).join(' · ') || 'No additional track information'}</span></div>{/each}</div></section>{/if}
      <section><h4>Reference</h4><div class="id-row"><code>{$mediaSelection.itemId}</code><button class="secondary" onclick={copyId}>{copied ? 'Copied' : 'Copy ID'}</button></div>{#if $mediaSelection.at}<p class="text-secondary text-xs">Observed {new Date($mediaSelection.at).toLocaleString()}</p>{/if}{#if item?.fetched_at}<p class="text-secondary text-xs">Metadata fetched {new Date(item.fetched_at).toLocaleString()}</p>{/if}</section>
    </div>
  {/if}
</dialog>
<style>
  .media-drawer { box-sizing:border-box; position:fixed; inset:0 0 0 auto; margin:0; padding:0; height:100dvh; max-height:100dvh; width:530px; max-width:100vw; color:var(--text-primary); background:var(--panel-bg); border:0; border-left:1px solid var(--border-color); box-shadow:-12px 0 80px #0008; }
  .media-drawer[open] { display:flex; flex-direction:column; } .media-drawer::backdrop { background:#0009; } .drawer-header { flex-shrink:0; } .drawer-body { min-height:0; }
  .hero { display:grid; grid-template-columns:115px 1fr; gap:22px; align-items:center; margin-bottom:24px; }.cover { height:168px; background:#242429; border:1px solid var(--border-color); border-radius:4px; display:flex; align-items:center; justify-content:center; overflow:hidden; }.cover img { height:100%; width:100%; object-fit:cover; }.cover span { font-size:10px; color:var(--text-secondary); letter-spacing:.12em; }
  h3 { font-size:23px; line-height:1.25; margin:10px 0; overflow-wrap:anywhere; } .eyebrow,h4 { text-transform:uppercase; font-size:10px; letter-spacing:.1em; color:var(--text-secondary); } h4 { margin:0 0 14px; } .subtitle { font-size:12px; color:var(--text-secondary); line-height:1.6; } .rating { border:1px solid var(--border-color); padding:3px 6px; margin-right:6px; font-size:11px; display:inline-block; } section { margin:24px 0; border-top:1px solid var(--border-color); padding-top:20px; }.overview { font-size:13px; line-height:1.8; white-space:pre-line; } .notice { border:1px solid #675628; background:#41351933; color:#dcc992; font-size:12px; padding:12px; line-height:1.6; }.tags { display:flex; flex-wrap:wrap; gap:6px; }.tags span { font-size:11px; background:#27272a; padding:4px 8px; border-radius:3px; }.facts { display:grid; grid-template-columns:1fr 1fr; gap:16px; }.facts div,.tracks div { display:flex; flex-direction:column; gap:6px; font-size:12px; }.facts span,.tracks span { color:var(--text-secondary); font-size:11px; }.facts strong,.tracks strong { font-weight:500; }.tracks { display:grid; gap:16px; }.id-row { display:flex; justify-content:space-between; gap:12px; align-items:center; }.id-row code { overflow-wrap:anywhere; font-size:12px; }
  @media(max-width:600px) { .media-drawer { width:100vw; }.hero { grid-template-columns:88px 1fr; gap:16px; }.cover { height:132px; }h3 { font-size:20px; } }
</style>
