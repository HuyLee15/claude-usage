package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"sync"
	"time"
)

// minManualRefresh throttles the dashboard's refresh button so a reload storm cannot hammer the endpoint.
const minManualRefresh = 10 * time.Second

type dashboard struct {
	cli      *client
	interval time.Duration

	mu      sync.RWMutex
	results []result
	updated time.Time
	err     error
}

type pageData struct {
	Rows           []result
	Updated        string
	RefreshSeconds int
	Err            error
}

var pageTemplate = template.Must(template.New("page").Funcs(template.FuncMap{
	"pct":    pct,
	"resets": resets,
	"level":  level,
	"ago":    formatAgo,
}).Parse(pageHTML))

func serve(ctx context.Context, cli *client, addr string, interval time.Duration) error {
	d := &dashboard{cli: cli, interval: interval}
	d.refresh(ctx, false)
	if errors.Is(d.err, errNoAccounts) {
		return d.err
	}
	go d.poll(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", d.handleIndex)
	mux.HandleFunc("GET /api/usage", d.handleJSON)
	mux.HandleFunc("POST /refresh", d.handleRefresh)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	fmt.Printf("dashboard: http://%s  (polling every %s, Ctrl+C to stop)\n", addr, interval)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve %s: %w", addr, err)
	}
	return nil
}

func (d *dashboard) poll(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.refresh(ctx, false)
		case <-ctx.Done():
			return
		}
	}
}

// refresh keeps the previous rows when a whole cycle fails, so a transient error does not blank the page.
func (d *dashboard) refresh(ctx context.Context, force bool) {
	results, err := d.cli.snapshot(ctx, force)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.updated, d.err = time.Now(), err
	if err != nil {
		fmt.Fprintln(os.Stderr, "refresh:", err)
		return
	}
	d.results = results
}

func (d *dashboard) snapshot() ([]result, time.Time, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.results, d.updated, d.err
}

func (d *dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer

	results, updated, err := d.snapshot()
	execErr := pageTemplate.Execute(&buf, pageData{
		Rows:           results,
		Updated:        updated.Format("15:04:05"),
		RefreshSeconds: int(d.interval.Seconds()),
		Err:            err,
	})
	if execErr != nil {
		http.Error(w, execErr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (d *dashboard) handleJSON(w http.ResponseWriter, r *http.Request) {
	results, _, _ := d.snapshot()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(toJSON(results)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (d *dashboard) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if _, updated, _ := d.snapshot(); time.Since(updated) >= minManualRefresh {
		// A client that disconnects mid-refresh must not leave cancelled results in the snapshot.
		d.refresh(context.WithoutCancel(r.Context()), true)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="{{.RefreshSeconds}}">
<title>Claude usage</title>
<style>
  :root { color-scheme: dark; }
  body { font-family: system-ui, sans-serif; margin: 2rem; background: #111; color: #eee; }
  h1 { font-size: 1.4rem; margin: 0 0 .5rem; }
  table { border-collapse: collapse; width: 100%; }
  th, td { padding: .5rem .75rem; text-align: left; border-bottom: 1px solid #333; white-space: nowrap; }
  th { color: #999; font-weight: 600; font-size: .8rem; text-transform: uppercase; }
  .bar { display: inline-block; width: 160px; height: 10px; background: #333; border-radius: 5px; vertical-align: middle; margin-left: .5rem; }
  .bar i { display: block; height: 100%; border-radius: 5px; }
  .ok i { background: #3fb950; } .warn i { background: #d29922; } .crit i { background: #f85149; }
  .err { color: #f85149; } .muted { color: #888; }
  form { display: inline; } button { background: #333; color: #eee; border: 1px solid #555; border-radius: 4px; padding: .2rem .6rem; cursor: pointer; }
</style>
</head>
<body>
<h1>Claude usage</h1>
{{if .Err}}<p class="err">last refresh failed: {{.Err}}</p>{{end}}
<p class="muted">updated {{.Updated}} · auto-refresh every {{.RefreshSeconds}}s · <form method="post" action="/refresh"><button>refresh now</button></form></p>
<table>
<tr><th>Account</th><th>Email</th><th>5h</th><th>Resets</th><th>Week</th><th>Resets</th><th>Sonnet wk</th><th>Opus wk</th><th>Status</th></tr>
{{range .Rows}}<tr>
<td>{{.Account.Name}}</td><td class="muted">{{.Account.Email}}</td>
{{if .Usage}}
<td>{{template "win" .Usage.FiveHour}}</td><td>{{resets .Usage.FiveHour}}</td>
<td>{{template "win" .Usage.SevenDay}}</td><td>{{resets .Usage.SevenDay}}</td>
<td>{{pct .Usage.SevenDaySonnet}}</td><td>{{pct .Usage.SevenDayOpus}}</td>
{{else}}<td colspan="6" class="muted">no data yet</td>{{end}}
<td>{{if .Err}}<span class="err">{{if .Usage}}as of {{ago .FetchedAt}} ago: {{end}}{{.Err}}</span>{{else}}<span class="muted">live</span>{{end}}</td>
</tr>
{{end}}</table>
</body>
</html>
{{define "win"}}{{if .}}{{pct .}}<span class="bar {{level .Utilization}}"><i style="width: {{printf "%.0f" .Utilization}}%"></i></span>{{else}}—{{end}}{{end}}
`
