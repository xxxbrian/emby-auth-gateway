<script lang="ts">
  import type { SeriesPoint } from './types';

  let {
    series = [],
    color = 'currentColor',
    height = 64,
    gapAware = false,
  }: {
    series?: SeriesPoint[];
    color?: string;
    height?: number;
    gapAware?: boolean;
  } = $props();

  const maxPoints = 120;

  /** Build path segments, breaking at gaps when gapAware. */
  let segments = $derived.by(() => {
    const raw = (series ?? []).filter(
      (p) => p != null && typeof p.v === 'number' && Number.isFinite(p.v),
    );
    if (raw.length < 2) return [];

    // Bucket-max downsample so sparse spikes are not skipped.
    let pts = raw;
    if (raw.length > maxPoints) {
      const bucket = Math.ceil(raw.length / maxPoints);
      pts = [];
      for (let i = 0; i < raw.length; i += bucket) {
        let best = raw[i];
        for (let j = i; j < Math.min(raw.length, i + bucket); j++) {
          if (Math.abs(raw[j].v) > Math.abs(best.v)) best = raw[j];
        }
        pts.push(best);
      }
    }

    let max = -Infinity;
    let min = Infinity;
    for (const p of pts) {
      if (p.v > max) max = p.v;
      if (p.v < min) min = p.v;
    }
    if (!Number.isFinite(max) || !Number.isFinite(min)) return [];

    // Vertical padding so flat/zero series stay visible mid-chart.
    if (max === min) {
      const pad = Math.abs(max) > 0 ? Math.abs(max) * 0.2 : 1;
      max += pad;
      min -= pad;
    } else {
      const pad = (max - min) * 0.12;
      max += pad;
      min -= pad;
    }
    const range = max - min || 1;

    // Build continuous segments, breaking at gaps.
    const result: string[] = [];
    let current: string[] = [];

    for (let i = 0; i < pts.length; i++) {
      const p = pts[i];
      // Gap detection: if gapAware and point has a gap marker
      const isGap = gapAware && (p as SeriesPoint & { gap?: boolean }).gap;
      if (isGap) {
        if (current.length >= 2) {
          result.push(current.join(' '));
        }
        current = [];
        continue;
      }

      const x = (i / (pts.length - 1)) * 100;
      const y = 100 - ((p.v - min) / range) * 100;
      const cmd = current.length === 0 ? 'M' : 'L';
      current.push(`${cmd}${x.toFixed(2)} ${y.toFixed(2)}`);
    }
    if (current.length >= 2) {
      result.push(current.join(' '));
    }

    return result;
  });

  /** Single combined path for backward compat (non-gap-aware). */
  let pathD = $derived(segments.length > 0 ? segments.join(' ') : '');

  let count = $derived((series ?? []).length);
  let gapCount = $derived.by(() => {
    if (!gapAware) return 0;
    return (series ?? []).filter(p => (p as SeriesPoint & { gap?: boolean }).gap).length;
  });
  let peak = $derived.by(() => {
    let m = 0;
    for (const p of series ?? []) {
      if (typeof p?.v === 'number' && p.v > m) m = p.v;
    }
    return m;
  });

  let titleText = $derived(
    gapAware && gapCount > 0
      ? `${count} samples \u00b7 ${gapCount} gaps \u00b7 peak ${peak}`
      : `${count} samples \u00b7 peak ${peak}`
  );
</script>

<div class="chart" style="height: {height}px;" title={titleText} role="img" aria-label={titleText}>
  {#if pathD}
    <svg width="100%" height="100%" viewBox="0 0 100 100" preserveAspectRatio="none" aria-hidden="true">
      <line x1="0" y1="50" x2="100" y2="50" stroke="var(--border-color)" stroke-width="0.5" vector-effect="non-scaling-stroke" />
      {#each segments as seg}
        <path d={seg} fill="none" stroke={color} stroke-width="1.5" vector-effect="non-scaling-stroke" />
      {/each}
    </svg>
  {:else}
    <div class="empty">no data</div>
  {/if}
</div>

<style>
  .chart {
    width: 100%;
    border: 1px solid var(--border-color);
    background: #0a0a0a;
    box-sizing: border-box;
  }
  .chart svg {
    display: block;
  }
  .empty {
    height: 100%;
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--text-secondary);
    font-size: 11px;
    font-family: var(--font-mono);
  }
</style>
