package server

import (
	"aegisgo/internal/tools"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DashboardDeps is the read-only slice the mini status page needs.
type DashboardDeps struct {
	// Stats computes the snapshot; nil renders a stub page (still 200 —
	// the dashboard is a liveness surface, not a dependency).
	Stats StatsSource
	// StartedAt anchors the uptime line (zero = omitted).
	StartedAt time.Time
	// Commit is the build identity shown in the footer.
	Commit string
}

// dashboardHandler renders GET / — one self-contained HTML page, no assets,
// no JS frameworks (stdlib-only rule). Auto-refresh via meta tag so a phone
// browser left open stays current without interaction.
func dashboardHandler(d DashboardDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="30">
<title>AegisGo status</title>
<style>
body{font:14px/1.5 -apple-system,system-ui,sans-serif;margin:2rem auto;max-width:34rem;padding:0 1rem;color:#111}
h1{font-size:1.1rem} .card{border:1px solid #ddd;border-radius:8px;padding:.8rem 1rem;margin:.6rem 0}
.kv{display:flex;justify-content:space-between} .kv b{font-variant-numeric:tabular-nums}
.muted{color:#666;font-size:.85rem}
</style></head><body>
<h1>🩺 AegisGo</h1>
`)
		if !d.StartedAt.IsZero() {
			fmt.Fprintf(&b, `<div class="card"><div class="kv"><span>uptime</span><b>%s</b></div></div>`+"\n",
				time.Since(d.StartedAt).Round(time.Second))
		}
		if d.Stats != nil {
			if snap, err := d.Stats.Stats(r.Context()); err == nil {
				fmt.Fprintf(&b, `<div class="card"><div class="kv"><span>runs</span><b>%d</b></div><div class="kv"><span>deflection</span><b>%.1f%%</b></div></div>`+"\n",
					snap.TotalRuns, snap.DeflectionRate*100)
				if len(snap.BySource) > 0 {
					var rows strings.Builder
					for _, src := range []string{"regex_router", "llm_classifier", "llm", "llm_disabled", "error"} {
						if n, ok := snap.BySource[src]; ok {
							fmt.Fprintf(&rows, `<div class="kv"><span>%s</span><b>%d</b></div>`, src, n)
						}
					}
					b.WriteString(`<div class="card">` + rows.String() + `</div>` + "\n")
				}
				if len(snap.RulesByState) > 0 {
					var rows strings.Builder
					for _, st := range []string{"seed", "active", "mined", "shadow"} {
						if n, ok := snap.RulesByState[st]; ok {
							fmt.Fprintf(&rows, `<div class="kv"><span>%s rules</span><b>%d</b></div>`, st, n)
						}
					}
					b.WriteString(`<div class="card">` + rows.String() + `</div>` + "\n")
				}
			}
		}
		// Live feed card: hidden until the first event arrives, so the page
		// reads identically in no-JS contexts (curl, ancient browsers).
		b.WriteString(`<div class="card" id="live" hidden>` + "\n" +
			`<div class="kv"><span>live feed</span><b id="live-count">listening</b></div>` + "\n" +
			`<div id="feed"></div>` + "\n" +
			`</div>` + "\n")
		fmt.Fprintf(&b, `<p class="muted">build %s · auto-refresh 30s · /healthz /readyz /v1/stats</p>`+"\n", d.Commit)
		b.WriteString(`<script>
(function(){
var f=document.getElementById('feed'),card=document.getElementById('live');
if(!f||!window.EventSource)return; // degrade silently: the page is complete without JS
var n=0;
var es=new EventSource('/v1/events');
es.addEventListener('run',function(e){
try{
var d=JSON.parse(e.data);
var label={regex_router:'Router',llm_classifier:'Classifier',llm:'LLM',llm_disabled:'LLM off',error:'Error'}[d.decision_source]||d.decision_source;
var row=document.createElement('div');row.className='kv';
var span=document.createElement('span');span.textContent=label+(d.rule_id?' \u00b7 '+d.rule_id:'');
var b=document.createElement('b');b.textContent=d.latency_ms+' ms';
row.appendChild(span);row.appendChild(b);
if(f.children.length >= 8)f.removeChild(f.lastElementChild); // bounded list, Hard Rule 10 on the page
f.insertBefore(row,f.firstElementChild); // newest first
n++;var c=document.getElementById('live-count');if(c)c.textContent=n+' runs this visit';
card.hidden=false;
}catch(err){}
});
})();
</script>` + "\n" + `</body></html>`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, b.String())
	}
}

// Job re-exports the tools.Job snapshot for Deps.Jobs consumers.
type Job = tools.Job
