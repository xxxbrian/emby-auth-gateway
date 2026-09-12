<script lang="ts">
  export interface ChartSeries { label: string; color: string; points: { t: string; v: number; gap?: boolean }[] }
  let { series = [], unit = 'count', label = 'History' }: { series?: ChartSeries[]; unit?: 'bytes' | 'count'; label?: string } = $props();
  let selected = $state<number | null>(null);
  let width = $state(640);
  let left = $derived(unit === 'bytes' ? 72 : 50);
  let right = $derived(Math.max(left + 1, width - 12));
  const top = 12, bottom = 166, height = 196;
  function measure(node: HTMLElement) {
    const resize = new ResizeObserver(entries => {
      const measured = entries[0]?.contentRect.width;
      if (measured > 0) width = measured;
    });
    resize.observe(node);
    return { destroy() { resize.disconnect(); } };
  }
  let points = $derived(series[0]?.points || []);
  let start = $derived(Date.parse(points[0]?.t || '') || 0);
  let end = $derived(Date.parse(points.at(-1)?.t || '') || start + 1);
  let max = $derived(Math.max(1, ...series.flatMap(s => s.points.filter(p => !p.gap && Number.isFinite(p.v)).map(p => p.v))));
  let hasData = $derived(series.some(s => s.points.some(p => !p.gap)));
  const x = (t: string) => left + (Date.parse(t) - start) / (end - start || 1) * (right - left);
  const y = (v: number) => bottom - v / max * (bottom - top);
  function format(v: number): string {
    if (unit === 'count') return Number.isInteger(v) ? v.toLocaleString() : v.toFixed(1);
    if (v >= 1024 ** 3) return `${(v / 1024 ** 3).toFixed(1)} GiB`;
    if (v >= 1024 ** 2) return `${(v / 1024 ** 2).toFixed(1)} MiB`;
    if (v >= 1024) return `${(v / 1024).toFixed(1)} KiB`;
    return `${Math.round(v)} B`;
  }
  function path(data: ChartSeries['points']): string {
    let d = '', gap = true;
    for (const p of data) {
      if (p.gap || !Number.isFinite(p.v)) { gap = true; continue; }
      // Keep every timestamp and every missing sample; no interpolation across gaps.
      const point = `${x(p.t).toFixed(2)},${y(p.v).toFixed(2)}`;
      d += gap ? `M${point}L${point} ` : `L${point} `; gap = false;
    }
    return d;
  }
  function inspect(event: PointerEvent) {
    const rect = (event.currentTarget as SVGElement).getBoundingClientRect();
    const target = start + Math.max(0, Math.min(1, ((event.clientX - rect.left) / rect.width * width - left) / (right - left))) * (end - start);
    let nearest = 0, distance = Infinity;
    points.forEach((p, i) => { const d = Math.abs(Date.parse(p.t) - target); if (d < distance) { distance = d; nearest = i; } });
    selected = nearest;
  }
  let current = $derived(Math.min(selected ?? Math.max(0, points.length - 1), Math.max(0, points.length - 1)));
  const time = (ms: number) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
</script>
<figure use:measure aria-label={label}>
  <div class="legend">{#each series as item}<span><i style:background={item.color}></i>{item.label}</span>{/each}</div>
  <svg viewBox={`0 0 ${width} ${height}`} height={height} role="img" aria-label={`${label}. ${points.length} timestamped samples. Use the sample slider below to inspect values.`} onpointermove={inspect} onpointerdown={inspect}>
    {#each [0, .5, 1] as fraction}<line x1={left} x2={right} y1={y(max * fraction)} y2={y(max * fraction)} class="grid"/><text x={left - 8} y={y(max * fraction) + 3} text-anchor="end">{format(max * fraction)}</text>{/each}
    {#if hasData}{#each series as item}<path d={path(item.points)} fill="none" stroke={item.color} stroke-width="1.7" stroke-linecap="round" vector-effect="non-scaling-stroke" />{/each}{:else}<text x={width / 2} y="85" text-anchor="middle"><tspan x={width / 2}>No committed samples</tspan><tspan x={width / 2} dy="16">in this window</tspan></text>{/if}
    {#each [0, .5, 1] as fraction}<text x={left + fraction * (right - left)} y="187" text-anchor={fraction === 0 ? 'start' : fraction === 1 ? 'end' : 'middle'}>{start ? time(start + fraction * (end - start)) : '—'}</text>{/each}
    {#if selected != null && points[current]}<line x1={x(points[current].t)} x2={x(points[current].t)} y1={top} y2={bottom} stroke="#a1a1aa" stroke-dasharray="3 3"/>{/if}
  </svg>
  {#if points.length}<input type="range" min="0" max={points.length - 1} value={current} oninput={(event) => selected = Number(event.currentTarget.value)} aria-label={`Inspect ${label} sample`} aria-valuetext={new Date(points[current].t).toLocaleString()} />{/if}
  <figcaption aria-live="polite"><time>{points[current] ? new Date(points[current].t).toLocaleString() : 'No samples'}</time><span>{#each series as item}<span class="reading"><i style:background={item.color}></i>{item.label}: {item.points[current] && !item.points[current].gap ? format(item.points[current].v) : 'Not observed'}</span>{/each}</span></figcaption>
</figure>
<style>
  figure { margin:0; min-width:0; }.legend { display:flex; gap:14px; flex-wrap:wrap; color:var(--text-secondary); font-size:11px; margin:0 0 10px; }.legend span,.reading { display:inline-flex; align-items:center; gap:5px; }i { width:7px; height:7px; border-radius:2px; flex-shrink:0; }svg { display:block; width:100%; overflow:hidden; touch-action:pan-y; flex-shrink:0; }text { fill:var(--text-secondary); font-size:11px; font-family:var(--font-mono); }.grid { stroke:var(--border-color); stroke-width:1; }input[type=range] { padding:0; height:12px; margin:4px 0 10px; accent-color:#648de6; }figcaption { display:flex; flex-direction:column; gap:7px; min-height:50px; font-size:11px; color:var(--text-secondary); }figcaption > span { display:flex; gap:5px 14px; flex-wrap:wrap; }time { font-family:var(--font-mono); color:var(--text-primary); }
</style>
