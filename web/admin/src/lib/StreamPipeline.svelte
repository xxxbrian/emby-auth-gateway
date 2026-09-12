<script lang="ts">
  import type { BufferStream } from './types';
  import { bufferBytes as bytes, streamStatus, conditionNames } from './buffer-display';
  let { stream }: { stream: BufferStream } = $props();
  let capacity = $derived(stream.private_base_bytes + stream.target_bytes);
  let owned = $derived(stream.private_base_bytes + stream.owned_bytes);
  let scale = $derived(Math.max(1, capacity, owned, stream.queued_bytes));
  let transient = $derived(stream.queued_bytes + stream.writing_bytes > owned || stream.queued_bytes > capacity);
  let upstreamWait = $derived(stream.wait_condition === 'upstream_stall' || stream.wait_condition === 'consumer_starvation');
  let queueWait = $derived(['buffer_acquire', 'pool_contention'].includes(stream.wait_condition) || stream.producer_state === 'waiting_for_buffer');
  let clientWait = $derived(stream.wait_condition === 'downstream_stall');
</script>
<div class="pipeline" class:warning={stream.health === 'warning'} class:critical={stream.health === 'critical'}>
  <div class="summary"><span class="health">{stream.health}</span><strong>{streamStatus(stream)}</strong></div>
  <div class="nodes">
    <div class="node" class:waiting={upstreamWait}><span class="step">01 · Upstream</span><strong>{stream.producer_state.startsWith('reading') ? 'Reading media' : stream.producer_state === 'done' ? 'Read complete' : 'Read on hold'}</strong><span class="count">{bytes(stream.bytes_read)}</span><small>Cumulative bytes read</small></div>
    <span class="arrow" aria-hidden="true">→</span>
    <div class="node queue" class:waiting={queueWait}><span class="step">02 · Gateway queue</span><strong>{bytes(stream.queued_bytes)} <small>queued now</small></strong><div class="queue-bar" class:uncertain={transient} role="img" aria-label={`${bytes(stream.queued_bytes)} queued; ${bytes(capacity)} allowance reference; ${bytes(owned)} allocated`}><span style:width={`${stream.queued_bytes / scale * 100}%`}></span><i style:left={`${Math.min(99.4, capacity / scale * 100)}%`}></i></div><small>Allowance {bytes(capacity)} · allocated {bytes(owned)}</small></div>
    <span class="arrow" aria-hidden="true">→</span>
    <div class="node" class:waiting={clientWait}><span class="step">03 · Client</span><strong>{stream.consumer_state === 'writing' ? 'Sending media' : stream.consumer_state === 'done' ? 'Write complete' : 'Waiting for data'}</strong><span class="count">{bytes(stream.bytes_written)}</span><small>Cumulative bytes sent · {bytes(stream.writing_bytes)} writing now</small></div>
  </div>
  {#if stream.wait_condition !== 'none'}<p class="wait">{conditionNames[stream.wait_condition] || stream.wait_condition} · {(stream.wait_duration_ms / 1000).toFixed(1)}s for this condition</p>{/if}
  <p class="note">Gateway memory reserve, measured in bytes. Allowance includes the private base and optional limit; it is not video or client playback progress.{transient ? ' Queue gauges and allocation currently differ; the bar is approximate.' : ' Queue gauges may settle after allocation changes.'}</p>
</div>
<style>
  .pipeline { --tone:#78a4fb; padding:20px; border:1px solid var(--border-color); background:#121216; border-radius:4px; margin-bottom:20px; }.warning { --tone:#d9b85d; }.critical { --tone:#f17979; }.summary { display:flex; gap:12px; align-items:center; margin-bottom:20px; flex-wrap:wrap; }.summary strong { font-size:13px; font-weight:500; }.health { color:var(--tone); border:1px solid color-mix(in srgb,var(--tone) 30%,transparent); background:color-mix(in srgb,var(--tone) 9%,transparent); padding:3px 7px; font-size:10px; text-transform:uppercase; border-radius:3px; }.nodes { display:grid; grid-template-columns:1fr 22px 1.35fr 22px 1fr; gap:10px; align-items:stretch; }.node { border:1px solid var(--border-color); padding:15px; display:flex; flex-direction:column; gap:9px; border-radius:4px; background:#19191e; min-width:0; }.node.waiting { border-color:var(--tone); background:color-mix(in srgb,var(--tone) 7%,#19191e); }.step { color:var(--text-secondary); text-transform:uppercase; font-size:9px; letter-spacing:.1em; }.node strong { font-size:12px; font-weight:500; }.node small { color:var(--text-secondary); font-size:10px; font-weight:400; line-height:1.5; }.count { font-family:var(--font-mono); font-size:18px; font-weight:400; margin-top:2px; }.arrow { color:var(--tone); display:flex; align-items:center; justify-content:center; font-size:20px; }.queue-bar { height:12px; background:#303038; position:relative; overflow:hidden; border-radius:2px; margin:6px 0; }.queue-bar span { display:block; height:100%; background:var(--tone); }.queue-bar i { position:absolute; top:0; bottom:0; border-left:2px solid #ededf0; }.queue-bar.uncertain { opacity:.5; }.note { font-size:10px; color:var(--text-secondary); margin:16px 0 0; line-height:1.7; }.wait { font-size:11px; color:var(--tone); margin:14px 0 0; }
  @media(max-width:750px) { .nodes { grid-template-columns:1fr; gap:7px; }.arrow { transform:rotate(90deg); height:16px; }.node { padding:12px; }.pipeline { padding:14px; } }
</style>
