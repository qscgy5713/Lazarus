package report

import (
	"fmt"
	"html"
	"io"
	"os"
	"strings"
	"time"

	"lazarus/internal/backup"
	"lazarus/internal/verify"
)

// HTML writes a standalone, self-contained, print-ready HTML disaster recovery
// audit report to w. It includes full verification details, schema analysis,
// RPO fallback metrics, and data integrity checks with zero external dependencies.
func HTML(w io.Writer, results []verify.Result, reportTitle string) error {
	if reportTitle == "" {
		reportTitle = "Lazarus Disaster Recovery Audit Report"
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "runner-host"
	}

	passed, failed := tally(results)
	total := passed + failed
	allPassed := failed == 0

	overallStatus := "ALL DRILLS PASSED"
	overallColor := "#10b981"
	overallBadgeBg := "#064e3b"
	if !allPassed {
		overallStatus = fmt.Sprintf("FAILED (%d ERRORS)", failed)
		overallColor = "#f43f5e"
		overallBadgeBg = "#4c0519"
	}

	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>%s</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background-color: #020617;
      color: #e2e8f0;
      line-height: 1.5;
      padding: 32px 16px;
    }
    .container { max-width: 1000px; margin: 0 auto; }
    .header {
      background: #0f172a;
      border: 1px solid #1e293b;
      border-radius: 12px;
      padding: 24px;
      margin-bottom: 24px;
      display: flex;
      justify-content: space-between;
      align-items: center;
      flex-wrap: wrap;
      gap: 16px;
    }
    .title-group h1 { font-size: 22px; font-weight: 800; color: #f8fafc; letter-spacing: -0.5px; }
    .title-group p { font-size: 13px; color: #94a3b8; margin-top: 4px; }
    .badge {
      display: inline-block;
      padding: 6px 14px;
      border-radius: 9999px;
      font-size: 13px;
      font-weight: 700;
      letter-spacing: 0.5px;
    }
    .metrics-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
      gap: 16px;
      margin-bottom: 24px;
    }
    .metric-card {
      background: #0f172a;
      border: 1px solid #1e293b;
      border-radius: 10px;
      padding: 16px;
    }
    .metric-label { font-size: 11px; font-weight: 600; text-transform: uppercase; color: #64748b; letter-spacing: 0.5px; }
    .metric-val { font-size: 26px; font-weight: 800; color: #f8fafc; margin-top: 4px; }
    .table-container {
      background: #0f172a;
      border: 1px solid #1e293b;
      border-radius: 12px;
      overflow-x: auto;
      margin-bottom: 32px;
    }
    table { width: 100%%; border-collapse: collapse; font-size: 13px; text-align: left; }
    th {
      background: #1e293b;
      color: #94a3b8;
      font-weight: 600;
      padding: 12px 16px;
      text-transform: uppercase;
      font-size: 11px;
      letter-spacing: 0.5px;
      border-bottom: 1px solid #334155;
    }
    td { padding: 14px 16px; border-bottom: 1px solid #1e293b; vertical-align: top; }
    tr:last-child td { border-bottom: none; }
    .status-tag {
      font-size: 11px;
      font-weight: 700;
      padding: 3px 8px;
      border-radius: 6px;
      display: inline-block;
    }
    .status-pass { background: rgba(16, 185, 129, 0.15); color: #34d399; border: 1px solid rgba(16, 185, 129, 0.3); }
    .status-fail { background: rgba(244, 63, 94, 0.15); color: #fb7185; border: 1px solid rgba(244, 63, 94, 0.3); }
    .status-fallback { background: rgba(56, 189, 248, 0.15); color: #38bdf8; border: 1px solid rgba(56, 189, 248, 0.3); }
    .target-details { margin-top: 32px; space-y: 20px; }
    .target-card {
      background: #0f172a;
      border: 1px solid #1e293b;
      border-radius: 12px;
      padding: 20px;
      margin-bottom: 20px;
    }
    .target-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 14px; }
    .target-title { font-size: 16px; font-weight: 700; color: #f8fafc; font-family: monospace; }
    .alert-box {
      border-radius: 8px;
      padding: 12px 14px;
      font-size: 12px;
      margin: 10px 0;
      line-height: 1.6;
    }
    .alert-info { background: rgba(56, 189, 248, 0.1); border: 1px solid rgba(56, 189, 248, 0.25); color: #7dd3fc; }
    .alert-warn { background: rgba(245, 158, 11, 0.1); border: 1px solid rgba(245, 158, 11, 0.25); color: #fcd34d; }
    .alert-danger { background: rgba(244, 63, 94, 0.1); border: 1px solid rgba(244, 63, 94, 0.25); color: #fda4af; }
    .check-row {
      display: flex;
      justify-content: space-between;
      padding: 6px 0;
      border-bottom: 1px solid #1e293b;
      font-size: 12px;
      font-family: monospace;
    }
    .footer {
      text-align: center;
      margin-top: 40px;
      font-size: 12px;
      color: #64748b;
      border-top: 1px solid #1e293b;
      padding-top: 20px;
    }
    @media print {
      body { background: #fff; color: #000; padding: 0; }
      .header, .metric-card, .table-container, .target-card { background: #fff; border: 1px solid #ccc; color: #000; }
      th { background: #eee; color: #000; }
      td { border-bottom: 1px solid #eee; }
      .badge, .status-tag { border: 1px solid #000; }
    }
  </style>
</head>
<body>
  <div class="container">
    <div class="header">
      <div class="title-group">
        <h1>🛡️ %s</h1>
        <p>Verified on Host: <strong>%s</strong> &bull; Generated At: <strong>%s</strong></p>
      </div>
      <div>
        <span class="badge" style="background: %s; color: %s; border: 1px solid %s;">%s</span>
      </div>
    </div>

    <div class="metrics-grid">
      <div class="metric-card">
        <div class="metric-label">Total Databases</div>
        <div class="metric-val">%d</div>
      </div>
      <div class="metric-card">
        <div class="metric-label">Drills Passed</div>
        <div class="metric-val" style="color: #34d399;">%d</div>
      </div>
      <div class="metric-card">
        <div class="metric-label">Drills Failed</div>
        <div class="metric-val" style="color: %s;">%d</div>
      </div>
      <div class="metric-card">
        <div class="metric-label">SLA Compliance</div>
        <div class="metric-val" style="color: #818cf8;">%.1f%%%%</div>
      </div>
    </div>

    <div class="table-container">
      <table>
        <thead>
          <tr>
            <th>Target</th>
            <th>Status</th>
            <th>Stage</th>
            <th>Total Time</th>
            <th>Restore Time</th>
            <th>Backup Size</th>
            <th>Checks</th>
          </tr>
        </thead>
        <tbody>
`,
		html.EscapeString(reportTitle),
		html.EscapeString(reportTitle),
		html.EscapeString(hostname),
		time.Now().UTC().Format(time.RFC3339),
		overallBadgeBg, overallColor, overallColor, overallStatus,
		total, passed,
		func() string {
			if failed > 0 {
				return "#fb7185"
			}
			return "#94a3b8"
		}(),
		failed,
		func() float64 {
			if total == 0 {
				return 100.0
			}
			return float64(passed) / float64(total) * 100.0
		}(),
	)

	for _, r := range results {
		tagClass := "status-pass"
		tagLabel := "PASS"
		if !r.Passed {
			tagClass = "status-fail"
			tagLabel = "FAIL"
		} else if r.FallbackUsed {
			tagClass = "status-fallback"
			tagLabel = "FALLBACK"
		}

		restoreStr := "-"
		if r.RestoreDuration > 0 {
			restoreStr = fmt.Sprintf("%.2fs", r.RestoreDuration.Seconds())
		}
		backupSize := "-"
		if r.Backup != nil {
			backupSize = backup.HumanSize(r.Backup.Size)
		}
		checksPassed := 0
		for _, c := range r.Checks {
			if c.Passed {
				checksPassed++
			}
		}
		checksStr := fmt.Sprintf("%d/%d", checksPassed, len(r.Checks))
		if len(r.Checks) == 0 {
			checksStr = "none"
		}

		fmt.Fprintf(w, `          <tr>
            <td><strong>%s</strong></td>
            <td><span class="status-tag %s">%s</span></td>
            <td><code>%s</code></td>
            <td>%s</td>
            <td>%s</td>
            <td>%s</td>
            <td>%s</td>
          </tr>
`,
			html.EscapeString(r.Target),
			tagClass, tagLabel,
			html.EscapeString(string(r.Stage)),
			r.Duration.Round(time.Millisecond).String(),
			restoreStr,
			backupSize,
			checksStr,
		)
	}

	fmt.Fprintf(w, `        </tbody>
      </table>
    </div>

    <div class="target-details">
      <h2 style="font-size: 18px; margin-bottom: 16px; font-weight: 700;">Detailed Target Audit Ledger</h2>
`)

	for _, r := range results {
		fmt.Fprintf(w, `      <div class="target-card">
        <div class="target-header">
          <div class="target-title">%s</div>
          <div>
            %s
          </div>
        </div>
`,
			html.EscapeString(r.Target),
			func() string {
				if r.Passed {
					return `<span class="status-tag status-pass">PASSED</span>`
				}
				return `<span class="status-tag status-fail">FAILED</span>`
			}(),
		)

		if r.Backup != nil {
			fmt.Fprintf(w, `        <p style="font-size: 12px; color: #94a3b8; font-family: monospace; margin-bottom: 8px;">
          Artifact: %s &bull; Size: %s &bull; Age: %s
        </p>
`,
				html.EscapeString(r.Backup.Path),
				backup.HumanSize(r.Backup.Size),
				r.Backup.Age(time.Now()).Round(time.Minute).String(),
			)
		}

		if r.FallbackUsed {
			fmt.Fprintf(w, `        <div class="alert-box alert-info">
          <strong>🔄 Fallback Disaster Recovery Active:</strong> %s
        </div>
`, html.EscapeString(r.FallbackMessage))
		}

		if r.ChaosInjected {
			if r.ChaosPassed {
				fmt.Fprintf(w, `        <div class="alert-box alert-info">
          <strong>🧪 Chaos Resilience Verified:</strong> %s
        </div>
`, html.EscapeString(r.ChaosMessage))
			} else {
				fmt.Fprintf(w, `        <div class="alert-box alert-warn">
          <strong>🧪 Chaos Drill Alert:</strong> %s
        </div>
`, html.EscapeString(r.ChaosMessage))
			}
		}

		if r.SchemaDrift != nil {
			if r.SchemaDrift.HasCriticalDrift {
				fmt.Fprintf(w, `        <div class="alert-box alert-warn">
          <strong>⚠️ Schema Drift Detected:</strong> Verified %d tables. Missing tables: %v, Empty tables: %v
        </div>
`, r.SchemaDrift.TotalTables, r.SchemaDrift.MissingTables, r.SchemaDrift.EmptyTables)
			} else {
				fmt.Fprintf(w, `        <p style="font-size: 12px; color: #64748b; margin-bottom: 8px;">
          Schema Introspection: %d tables verified against baseline (zero table drift).
        </p>
`, r.SchemaDrift.TotalTables)
			}
		}

		if len(r.Checks) > 0 {
			fmt.Fprintf(w, `        <div style="margin-top: 12px;">
          <h4 style="font-size: 11px; text-transform: uppercase; color: #64748b; margin-bottom: 6px;">Data Assertions</h4>
`)
			for _, c := range r.Checks {
				resCol := "#34d399"
				resLabel := fmt.Sprintf("✓ OK (= %d)", c.Value)
				if !c.Passed {
					resCol = "#fb7185"
					resLabel = fmt.Sprintf("✗ FAIL: %s", c.Reason)
				}
				fmt.Fprintf(w, `          <div class="check-row">
            <span>%s</span>
            <span style="color: %s;">%s</span>
          </div>
`, html.EscapeString(c.Name), resCol, html.EscapeString(resLabel))
			}
			fmt.Fprintf(w, `        </div>
`)
		}

		if r.Err != nil {
			fmt.Fprintf(w, `        <div class="alert-box alert-danger">
          <strong>Drill Failure at Stage [%s]:</strong> %s
        </div>
`, html.EscapeString(string(r.Stage)), html.EscapeString(r.Err.Error()))
		}

		if r.LogsTail != "" {
			fmt.Fprintf(w, `        <details style="margin-top: 10px;">
          <summary style="font-size: 11px; cursor: pointer; color: #94a3b8; font-family: monospace;">Container Error Logs (tail)</summary>
          <pre style="margin-top: 6px; padding: 10px; background: #020617; border-radius: 6px; font-size: 11px; overflow-x: auto; color: #f87171;">%s</pre>
        </details>
`, html.EscapeString(strings.TrimSpace(r.LogsTail)))
		}

		fmt.Fprintf(w, `      </div>
`)
	}

	fmt.Fprintf(w, `    </div>

    <div class="footer">
      Generated automatically by <a href="https://github.com/chen-yuju/Lazarus" style="color: #818cf8; text-decoration: none;">Lazarus</a> &bull; Ephemeral Database Restoration Drills
    </div>
  </div>
</body>
</html>
`)

	return nil
}
