<script lang="ts">
    import type { TranscodingAggregate } from './types';
    let { value }: { value: TranscodingAggregate | undefined } = $props();
    function size(n: number): string {
        return n >= 1024 ** 3 ? `${(n / 1024 ** 3).toFixed(1)} GiB` : `${(n / 1024 ** 2).toFixed(0)} MiB`;
    }
</script>

<div class="panel conversion-summary">
    <div class="summary-heading">
        <span class="metric-label">Audio conversion</span>
        <a href="#/transcoding">View tasks →</a>
    </div>
    {#if value?.enabled}
        <div class="summary-values">
            <div><span class="text-secondary text-sm">Worker slots</span><strong class="mono">{value.running} / {value.worker_limit}</strong></div>
            <div><span class="text-secondary text-sm">Waiting</span><strong class="mono">{value.queued}</strong></div>
            <div><span class="text-secondary text-sm">Segment cache</span><strong class="mono">{size(value.cache_bytes)} <span class="text-secondary text-sm">/ {size(value.cache_budget_bytes)}</span></strong></div>
            <div><span class="text-secondary text-sm">Failed tasks</span><strong class:status-err={value.failed > 0} class="mono">{value.failed}</strong></div>
        </div>
    {:else}
        <p class="text-secondary">{value ? 'Disabled' : 'Conversion status unavailable'}</p>
    {/if}
</div>

<style>
    .summary-heading { display: flex; align-items: center; justify-content: space-between; gap: 1rem; margin-bottom: 1rem; }
    .summary-heading a { font-size: .8rem; }
    .summary-values { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 1.2rem; }
    .summary-values strong { display: block; font-size: 1.15rem; margin-top: .4rem; font-weight: 500; }
    p { margin: 0; font-size: .875rem; }
</style>
