package main

// A single self-contained dashboard page served at GET /ui.
//
// The mock has no web GUI of its own — real UFM does — so this gives the
// fabric, its partitions, events and alarms a live view during a demo. It
// polls the same REST API everything else uses; there is no special backend.

import "net/http"

func (s *server) handleUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mock UFM</title>
<style>
  :root {
    --bg:#0d1117; --panel:#161b22; --border:#30363d; --text:#e6edf3;
    --muted:#8b949e; --accent:#76b900; --crit:#f85149; --warn:#d29922;
    --info:#58a6ff; --mono:ui-monospace,SFMono-Regular,Menlo,monospace;
  }
  @media (prefers-color-scheme: light) {
    :root { --bg:#ffffff; --panel:#f6f8fa; --border:#d0d7de; --text:#1f2328;
            --muted:#656d76; }
  }
  * { box-sizing:border-box; }
  body { margin:0; background:var(--bg); color:var(--text);
         font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
  header { padding:16px 24px; border-bottom:1px solid var(--border);
           display:flex; align-items:center; gap:16px; flex-wrap:wrap; }
  h1 { margin:0; font-size:17px; font-weight:600; }
  h1 span { color:var(--accent); }
  .pill { font:12px/1 var(--mono); padding:5px 9px; border-radius:20px;
          background:var(--panel); border:1px solid var(--border); color:var(--muted); }
  main { padding:20px 24px; display:grid; gap:20px;
         grid-template-columns:repeat(auto-fit,minmax(430px,1fr)); }
  section { background:var(--panel); border:1px solid var(--border); border-radius:8px; }
  h2 { margin:0; padding:11px 15px; font-size:12px; text-transform:uppercase;
       letter-spacing:.6px; color:var(--muted); border-bottom:1px solid var(--border); }
  .wrap { overflow-x:auto; }
  table { width:100%; border-collapse:collapse; font:12px/1.5 var(--mono); }
  th { text-align:left; padding:7px 15px; color:var(--muted); font-weight:500;
       border-bottom:1px solid var(--border); white-space:nowrap; }
  td { padding:7px 15px; border-bottom:1px solid var(--border); white-space:nowrap; }
  tr:last-child td { border-bottom:none; }
  .empty { padding:20px 15px; color:var(--muted); font-style:italic; }
  .dot { display:inline-block; width:7px; height:7px; border-radius:50%; margin-right:7px; }
  .up { background:var(--accent); } .down { background:var(--crit); }
  .sev-Critical { color:var(--crit); font-weight:600; }
  .sev-Error    { color:var(--crit); }
  .sev-Warning  { color:var(--warn); }
  .sev-Info     { color:var(--info); }
  .stats { display:flex; gap:22px; padding:14px 15px; flex-wrap:wrap; }
  .stat b { display:block; font-size:21px; font-family:var(--mono); font-weight:600; }
  .stat span { font-size:11px; color:var(--muted); text-transform:uppercase;
               letter-spacing:.5px; }
  footer { padding:12px 24px 26px; color:var(--muted); font-size:12px; }
  code { font-family:var(--mono); background:var(--bg); padding:1px 5px;
         border-radius:4px; border:1px solid var(--border); }
</style>
</head>
<body>
<header>
  <h1><span>NVIDIA</span> UFM &mdash; mock</h1>
  <span class="pill" id="version">connecting…</span>
  <span class="pill" id="clock"></span>
</header>

<main>
  <section style="grid-column:1/-1">
    <h2>Fabric</h2>
    <div class="stats" id="stats"></div>
  </section>

  <section>
    <h2>InfiniBand ports</h2>
    <div class="wrap"><table id="ports"></table></div>
  </section>

  <section>
    <h2>Partitions (pkeys)</h2>
    <div class="wrap"><table id="pkeys"></table></div>
  </section>

  <section>
    <h2>Active alarms</h2>
    <div class="wrap"><table id="alarms"></table></div>
  </section>

  <section>
    <h2>Recent events</h2>
    <div class="wrap"><table id="events"></table></div>
  </section>
</main>

<footer>
  Polling every 2s. Token is read from <code>?token=</code> or defaults to
  <code>demo-token</code>.
</footer>

<script>
const token = new URLSearchParams(location.search).get('token') || 'demo-token';
const headers = { 'Authorization': 'Basic ' + token };

async function get(path) {
  const response = await fetch(path, { headers });
  if (!response.ok) throw new Error(path + ' -> ' + response.status);
  return response.json();
}

const esc = (value) => String(value ?? '')
  .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

function render(id, columns, rows, cells) {
  const table = document.getElementById(id);
  if (!rows.length) {
    table.innerHTML = '<tr><td class="empty">none</td></tr>';
    return;
  }
  table.innerHTML =
    '<tr>' + columns.map((c) => '<th>' + c + '</th>').join('') + '</tr>' +
    rows.map((row) => '<tr>' + cells(row).map((c) => '<td>' + c + '</td>').join('') + '</tr>').join('');
}

async function refresh() {
  try {
    const [version, ports, pkeys, alarms, events] = await Promise.all([
      get('/ufmRestV3/app/ufm_version'),
      get('/ufmRestV3/resources/ports'),
      get('/ufmRestV3/resources/pkeys?guids_data=true&qos_conf=true'),
      get('/ufmRestV3/app/alarms'),
      get('/ufmRestV3/app/events'),
    ]);

    document.getElementById('version').textContent = 'UFM ' + version.ufm_release_version;
    document.getElementById('clock').textContent = new Date().toLocaleTimeString();

    const active = ports.filter((p) => p.logical_state === 'Active').length;
    const critical = alarms.filter((a) => a.severity === 'Critical').length;
    document.getElementById('stats').innerHTML = [
      ['ports', ports.length], ['active', active],
      ['partitions', Object.keys(pkeys).length],
      ['alarms', alarms.length], ['critical', critical],
      ['events', events.length],
    ].map(([label, value]) =>
      '<div class="stat"><b>' + value + '</b><span>' + label + '</span></div>').join('');

    render('ports', ['GUID', 'LID', 'System', 'State'], ports, (p) => [
      esc(p.guid), p.lid, esc(p.system_name),
      '<span class="dot ' + (p.logical_state === 'Active' ? 'up' : 'down') + '"></span>' +
        esc(p.logical_state),
    ]);

    render('pkeys', ['pkey', 'Name', 'Members', 'Rate limit'],
      Object.entries(pkeys), ([key, value]) => [
        esc(key), esc(value.partition),
        (value.guids || []).length,
        value.qos_conf ? value.qos_conf.rate_limit : '—',
      ]);

    render('alarms', ['ID', 'Severity', 'Object', 'Name', 'Count', 'Duration'], alarms, (a) => [
      a.id, '<span class="sev-' + esc(a.severity) + '">' + esc(a.severity) + '</span>',
      esc(a.object_name), esc(a.name), a.event_count, esc(a.duration),
    ]);

    render('events', ['ID', 'Time', 'Severity', 'Object', 'Description'],
      events.slice(-15).reverse(), (e) => [
        e.id, esc(e.timestamp),
        '<span class="sev-' + esc(e.severity) + '">' + esc(e.severity) + '</span>',
        esc(e.object_name), esc(e.description),
      ]);
  } catch (error) {
    document.getElementById('version').textContent = String(error.message || error);
  }
}

refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>
`
