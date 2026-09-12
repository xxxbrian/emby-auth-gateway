<script lang="ts">
  import { mediaCache, mediaKey, mediaStatus, mediaSubtitle, openMedia, requestMedia, restoreLocalMedia } from './media';
  import type { MediaLocalState } from './types';
  let { itemId, fallbackName = null, sourceRef = null, at = null, userId = null, localState = null }: {
    itemId: string | null | undefined; fallbackName?: string | null; sourceRef?: string | null; at?: string | null; userId?: string | null; localState?: MediaLocalState | null;
  } = $props();
  let element = $state<HTMLButtonElement>();
  let visible = $state(false);
  let imageFailed = $state(false);
  let item = $derived($mediaCache.get(mediaKey(itemId || '', sourceRef))?.item);
  $effect(() => {
    const id = itemId; const source = sourceRef; imageFailed = false;
    if (!id || !visible) return;
    const ref={ itemId: id, sourceRef: source, fallbackName };
    let release=requestMedia(ref);
    const timer=setInterval(()=>{ release(); release=requestMedia(ref); },60_000);
    return ()=>{ release(); clearInterval(timer); };
  });
  $effect(() => { if(itemId && userId && localState) restoreLocalMedia({itemId,userId,localState,fallbackName}); });
  function observe(node: HTMLElement) {
    if (typeof IntersectionObserver === 'undefined') { visible = true; return; }
    const observer = new IntersectionObserver(entries => { visible = entries[0].isIntersecting; }, { rootMargin: '120px' });
    observer.observe(node); return { destroy() { observer.disconnect(); } };
  }
</script>
{#if itemId}
  <button bind:this={element} use:observe class="media-cell" onclick={() => openMedia({ itemId: itemId!, fallbackName, sourceRef, at, userId, localState }, element)} aria-label={`View media: ${item?.name || fallbackName || itemId}`}>
    <span class="poster" aria-hidden="true">
      {#if visible && item?.image && !imageFailed}<img src={item.image} alt="" loading="lazy" decoding="async" width="40" height="54" onerror={() => imageFailed = true} />{:else}<svg viewBox="0 0 24 24" fill="none"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="m10 8 6 4-6 4z"/></svg>{/if}
    </span>
    <span class="copy"><strong>{item?.name || fallbackName || 'Media details'}</strong><span>{mediaSubtitle(item) || (item?.status === 'available' ? item.type || '' : mediaStatus(item))}</span>{#if item?.stale}<span>Cached metadata</span>{/if}{#if !item?.name && !fallbackName}<code>{itemId}</code>{/if}</span>
  </button>
{:else}<span class="text-secondary">No media reference</span>{/if}
<style>
  .media-cell { display:flex; align-items:center; gap:10px; min-width:165px; max-width:340px; width:100%; text-align:left; color:var(--text-primary); background:transparent; border:0; padding:2px 0; font-size:12px; }
  .media-cell:hover strong { color:#93b4ff; text-decoration:underline; } .media-cell:focus-visible { outline:2px solid var(--accent); outline-offset:3px; }
  .poster { width:40px; height:54px; flex-shrink:0; display:flex; align-items:center; justify-content:center; background:#242429; border:1px solid var(--border-color); border-radius:3px; overflow:hidden; }
  .poster img { object-fit:cover; width:100%; height:100%; } .poster svg { width:22px; stroke:#777782; stroke-width:1; }
  .copy { display:flex; flex-direction:column; gap:4px; min-width:0; }.copy strong { font-weight:500; line-height:1.4; overflow-wrap:anywhere; }.copy span,.copy code { font-size:11px; font-weight:400; color:var(--text-secondary); white-space:normal; line-height:1.4; overflow-wrap:anywhere; }
  @media(max-width:600px) { .media-cell { min-width:0; max-width:100%; } }
</style>
