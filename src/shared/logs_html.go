package shared

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// RenderLogsHTML produces a self-refreshing HTML page showing log entries.
// serviceTitle is shown in the page heading (e.g. "RocketMan Service Logs").
func RenderLogsHTML(entries []LogEntry, currentSource string, retention time.Duration, serviceTitle string) string {
	var b strings.Builder

	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>` + html.EscapeString(serviceTitle) + `</title>`)
	b.WriteString("<style>")
	b.WriteString("body{font-family:Segoe UI,Arial,sans-serif;background:#f5f7fb;color:#1f2937;margin:0;padding:24px;}")
	b.WriteString(".wrap{max-width:1200px;margin:0 auto;background:#fff;border-radius:12px;box-shadow:0 8px 24px rgba(0,0,0,.08);overflow:hidden;}")
	b.WriteString(".head{padding:16px 20px;border-bottom:1px solid #e5e7eb;display:flex;justify-content:space-between;align-items:center;}")
	b.WriteString("h1{font-size:20px;margin:0;} .meta{font-size:13px;color:#6b7280;} .tools{display:flex;gap:15px;align-items:center;}")
	b.WriteString(".tools a{color:#2563eb;text-decoration:none;font-size:13px;}")
	b.WriteString("select{padding:4px 8px;border-radius:4px;border:1px solid #d1d5db;font-size:13px;outline:none;cursor:pointer;}")
	b.WriteString("table{width:100%;border-collapse:collapse;} th,td{padding:10px 12px;border-bottom:1px solid #f1f5f9;vertical-align:top;font-size:13px;}")
	b.WriteString("th{background:#f8fafc;text-align:left;color:#334155;font-weight:600;position:sticky;top:0;}")
	b.WriteString(".lvl{display:inline-block;padding:2px 8px;border-radius:999px;font-weight:600;font-size:11px;}")
	b.WriteString(".lvl-info{background:#dbeafe;color:#1d4ed8;} .lvl-error{background:#fee2e2;color:#b91c1c;} .lvl-warn{background:#ffedd5;color:#9a3412;}")
	b.WriteString(".src{font-size:11px;color:#6b7280;text-transform:uppercase;font-weight:bold;}")
	b.WriteString(".msg{white-space:pre-wrap;word-break:break-word;font-family:Consolas,monospace;}")
	b.WriteString("</style></head><body>")
	b.WriteString(`<div class="wrap"><div class="head"><div>`)
	b.WriteString(`<h1>` + html.EscapeString(serviceTitle) + `</h1>`)
	b.WriteString(fmt.Sprintf(`<div class="meta">Last %s &bull; Entries: %d &bull; Updated: %s</div>`,
		html.EscapeString(retention.String()),
		len(entries),
		html.EscapeString(time.Now().Format("2006-01-02 15:04:05"))))
	b.WriteString(`</div><div class="tools">`)

	// Source selector
	b.WriteString(`<select onchange="window.location.href='/logs?source='+this.value">`)
	for _, s := range []struct{ val, label string }{
		{"all", "All Sources"},
		{"main", "Main Service"},
		{"sing-box", "Sing-box"},
	} {
		selected := ""
		if currentSource == s.val {
			selected = " selected"
		}
		b.WriteString(fmt.Sprintf(`<option value="%s"%s>%s</option>`, s.val, selected, s.label))
	}
	b.WriteString("</select>")

	refreshURL := "/logs"
	if currentSource != "all" && currentSource != "" {
		refreshURL = "/logs?source=" + currentSource
	}
	jsonURL := "/logs?format=json"
	if currentSource != "all" && currentSource != "" {
		jsonURL += "&source=" + currentSource
	}

	b.WriteString(fmt.Sprintf(`<a href="%s">JSON</a> &bull; <a href="%s">Refresh</a></div></div>`, jsonURL, refreshURL))
	b.WriteString(`<table><thead><tr>`)
	b.WriteString(`<th style="width:150px">Time</th><th style="width:80px">Source</th><th style="width:80px">Level</th><th>Message</th>`)
	b.WriteString(`</tr></thead><tbody>`)

	for _, entry := range entries {
		lvlClass := "lvl-info"
		if strings.EqualFold(entry.Level, "ERROR") {
			lvlClass = "lvl-error"
		} else if strings.EqualFold(entry.Level, "WARN") {
			lvlClass = "lvl-warn"
		}

		b.WriteString("<tr>")
		b.WriteString("<td>" + html.EscapeString(entry.Time.Format("15:04:05.000")) + "</td>")
		b.WriteString(`<td><span class="src">` + html.EscapeString(entry.Source) + "</span></td>")
		b.WriteString(`<td><span class="lvl ` + lvlClass + `">` + html.EscapeString(strings.ToUpper(entry.Level)) + "</span></td>")
		b.WriteString(`<td class="msg">` + html.EscapeString(entry.Message) + "</td>")
		b.WriteString("</tr>")
	}

	b.WriteString("</tbody></table></div>")
	b.WriteString("<script>")
	b.WriteString("const params=new URLSearchParams(window.location.search);")
	b.WriteString("const source=params.get('source')||'all';")
	b.WriteString("setTimeout(function(){if(source==='all'||source==='main'||source==='sing-box')window.location.reload();},5000);")
	b.WriteString("</script>")
	b.WriteString("</body></html>")
	return b.String()
}
