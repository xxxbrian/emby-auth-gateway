<script lang="ts">
  let {
    owned = 0,
    free = 0,
    unallocated = 0,
    label = true,
  }: {
    owned?: number;
    free?: number;
    unallocated?: number;
    label?: boolean;
  } = $props();

  function fmtBytes(v: number): string {
    if (v <= 0) return '0 B';
    if (v < 1024) return `${v} B`;
    if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)} KB`;
    if (v < 1024 * 1024 * 1024) return `${(v / (1024 * 1024)).toFixed(1)} MB`;
    return `${(v / (1024 * 1024 * 1024)).toFixed(1)} GB`;
  }

  let total = $derived(owned + free + unallocated);
  let ownedPct = $derived(total > 0 ? (owned / total) * 100 : 0);
  let freePct = $derived(total > 0 ? (free / total) * 100 : 0);

  let ariaLabel = $derived(
    `Pool allocation: ${fmtBytes(owned)} owned, ${fmtBytes(free)} free, ${fmtBytes(unallocated)} unallocated of ${fmtBytes(total)} total`
  );
</script>

<div class="capacity-bar-wrap" role="img" aria-label={ariaLabel}>
  <div class="capacity-bar">
    {#if ownedPct > 0}
      <div class="segment segment-owned" style="width: {ownedPct}%"></div>
    {/if}
    {#if freePct > 0}
      <div class="segment segment-free" style="width: {freePct}%"></div>
    {/if}
  </div>
  {#if label}
    <div class="capacity-labels">
      <span class="capacity-label"><span class="dot dot-owned"></span> owned {fmtBytes(owned)}</span>
      <span class="capacity-label"><span class="dot dot-free"></span> free {fmtBytes(free)}</span>
      <span class="capacity-label"><span class="dot dot-unalloc"></span> unallocated {fmtBytes(unallocated)}</span>
    </div>
  {/if}
</div>

<style>
  .capacity-bar-wrap {
    margin: 0.5rem 0;
  }
  .capacity-bar {
    width: 100%;
    height: 8px;
    background-color: var(--bg-color);
    border: 1px solid var(--border-color);
    border-radius: 2px;
    display: flex;
    overflow: hidden;
  }
  .segment {
    height: 100%;
    min-width: 0;
  }
  .segment-owned {
    background-color: var(--text-primary);
  }
  .segment-free {
    background-color: var(--border-color);
  }
  .capacity-labels {
    display: flex;
    gap: 1rem;
    margin-top: 0.375rem;
    flex-wrap: wrap;
  }
  .capacity-label {
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--text-secondary);
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 1px;
    display: inline-block;
  }
  .dot-owned {
    background-color: var(--text-primary);
  }
  .dot-free {
    background-color: var(--border-color);
  }
  .dot-unalloc {
    background-color: var(--bg-color);
    border: 1px solid var(--border-color);
  }
</style>
