// Package notify sends the outcome of a verification run to a webhook.
//
// Without this, Lazarus only reports through its exit code and stdout — which
// means an unattended cron run that finds a broken backup tells nobody. The
// tool that exists to catch silent failures shouldn't have one of its own.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"lazarus/internal/backup"
	"lazarus/internal/config"
	"lazarus/internal/verify"
)

// Format shapes the webhook body.
type Format string

const (
	FormatSlack     Format = "slack"
	FormatDiscord   Format = "discord"
	FormatTelegram  Format = "telegram"
	FormatTeams     Format = "teams"
	FormatGeneric   Format = "generic"
	FormatLazarus   Format = "lazarus"
	FormatPagerDuty Format = "pagerduty"
	FormatEmail     Format = "email"
)

// When decides which runs are worth a message.
type When string

const (
	// WhenOnFailure notifies only when something is wrong.
	WhenOnFailure When = "on_failure"
	// WhenAlways notifies on every run. Useful as a heartbeat: if Lazarus
	// itself stops running, silence would otherwise look exactly like
	// "all backups are fine".
	WhenAlways When = "always"
	WhenNever  When = "never"
)

const maxMessageLen = 2000

type Notifier struct {
	url        string
	format     Format
	when       When
	apiKey     string
	client     *http.Client
	smtp       *config.SMTPConfig
	smtpSender func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

func New(url string, format Format, when When) *Notifier {
	return &Notifier{
		url:        url,
		format:     format,
		when:       when,
		client:     &http.Client{Timeout: 10 * time.Second},
		smtpSender: smtp.SendMail,
	}
}

// WithAPIKey attaches an authentication token/key to outgoing webhook requests.
func (n *Notifier) WithAPIKey(key string) *Notifier {
	if n != nil {
		n.apiKey = key
	}
	return n
}

// WithSMTP configures SMTP settings for email notifications.
func (n *Notifier) WithSMTP(cfg *config.SMTPConfig) *Notifier {
	if n != nil {
		n.smtp = cfg
	}
	return n
}

// ShouldSend reports whether this run's outcome warrants a message.
func (n *Notifier) ShouldSend(results []verify.Result) bool {
	if n == nil || n.when == WhenNever {
		return false
	}
	if n.format == FormatEmail {
		if n.smtp == nil {
			return false
		}
	} else if n.url == "" {
		return false
	}

	if n.when == WhenAlways {
		return true
	}
	return anyFailed(results)
}

// Send delivers the summary. A delivery failure is returned rather than
// swallowed so the caller can surface it — a notifier that quietly stops
// working recreates the exact blind spot this package exists to close.
// Network glitches or 5xx server errors trigger up to 3 attempts with exponential backoff.
func (n *Notifier) Send(ctx context.Context, results []verify.Result) error {
	if n.format == FormatEmail {
		return n.sendEmail(ctx, results)
	}

	payload, err := n.buildPayload(results)
	if err != nil {
		return err
	}

	maxAttempts := 3
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("build webhook request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if n.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+n.apiKey)
			req.Header.Set("X-Lazarus-Key", n.apiKey)
		}

		resp, err := n.client.Do(req)
		if err == nil {
			status := resp.StatusCode
			_ = resp.Body.Close()
			if status < 300 {
				return nil
			}
			lastErr = fmt.Errorf("webhook returned status %d", status)
			// Do not retry 4xx errors other than 429 Too Many Requests
			if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
				return lastErr
			}
		} else {
			lastErr = fmt.Errorf("send webhook: %w", err)
		}

		if attempt < maxAttempts {
			backoff := time.Duration(100*(1<<(attempt-1))) * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return lastErr
}

func (n *Notifier) buildPayload(results []verify.Result) ([]byte, error) {
	switch n.format {
	case FormatDiscord:
		return json.Marshal(buildDiscord(results))
	case FormatTelegram:
		return json.Marshal(n.buildTelegram(results))
	case FormatTeams:
		return json.Marshal(buildTeams(results))
	case FormatPagerDuty:
		return json.Marshal(n.buildPagerDuty(results))
	case FormatGeneric, FormatLazarus:
		return json.Marshal(buildGeneric(results))
	default: // Slack, and anything Slack-compatible (Mattermost, etc.)
		return json.Marshal(buildSlack(results))
	}
}

type pagerDutyPayload struct {
	RoutingKey  string           `json:"routing_key"`
	EventAction string           `json:"event_action"`
	Payload     pagerDutyDetails `json:"payload"`
}

type pagerDutyDetails struct {
	Summary       string         `json:"summary"`
	Severity      string         `json:"severity"`
	Source        string         `json:"source"`
	Timestamp     string         `json:"timestamp"`
	CustomDetails map[string]any `json:"custom_details,omitempty"`
}

func (n *Notifier) buildPagerDuty(results []verify.Result) pagerDutyPayload {
	hasFailure := anyFailed(results)
	failedCount := 0
	for _, r := range results {
		if !r.Passed {
			failedCount++
		}
	}

	action := "trigger"
	severity := "critical"
	summary := fmt.Sprintf("Lazarus backup drill: %d target(s) failed", failedCount)
	if !hasFailure {
		severity = "info"
		summary = fmt.Sprintf("Lazarus backup drill: all %d target(s) passed", len(results))
	}

	routingKey := n.apiKey
	if routingKey == "" {
		parts := strings.Split(strings.TrimRight(n.url, "/"), "/")
		routingKey = parts[len(parts)-1]
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "lazarus"
	}

	return pagerDutyPayload{
		RoutingKey:  routingKey,
		EventAction: action,
		Payload: pagerDutyDetails{
			Summary:   summary,
			Severity:  severity,
			Source:    host,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			CustomDetails: map[string]any{
				"total_targets": len(results),
				"failed":        failedCount,
				"results":       buildGeneric(results),
			},
		},
	}
}

type slackPayload struct {
	Text   string       `json:"text"`
	Blocks []slackBlock `json:"blocks,omitempty"`
}

type slackBlock struct {
	Type   string         `json:"type"`
	Text   *slackTextObj  `json:"text,omitempty"`
	Fields []slackTextObj `json:"fields,omitempty"`
}

type slackTextObj struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func buildSlack(results []verify.Result) slackPayload {
	summaryText := FormatMessage(results)
	failed := anyFailed(results)

	headerText := "✅ Lazarus: All Database Restorations Passed"
	if failed {
		headerText = "🚨 Lazarus: Database Restoration Verification Failed"
	}

	blocks := []slackBlock{
		{
			Type: "header",
			Text: &slackTextObj{Type: "plain_text", Text: headerText},
		},
		{
			Type: "section",
			Text: &slackTextObj{Type: "mrkdwn", Text: fmt.Sprintf("*Status Summary:*\n```\n%s\n```", truncate(summaryText))},
		},
	}

	return slackPayload{
		Text:   summaryText,
		Blocks: blocks,
	}
}

type discordPayload struct {
	Content string         `json:"content"`
	Embeds  []discordEmbed `json:"embeds,omitempty"`
}

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

func buildDiscord(results []verify.Result) discordPayload {
	summaryText := FormatMessage(results)
	failed := anyFailed(results)

	color := 0x2ecc71 // green
	title := "✅ Lazarus: Restoration Drills Passed"
	if failed {
		color = 0xe74c3c // red
		title = "🚨 Lazarus: Restoration Drill Failed"
	}

	var fields []discordField
	for _, r := range results {
		status := "🟢 PASS"
		val := fmt.Sprintf("Restored in %s", r.RestoreDuration.Truncate(time.Millisecond))
		if !r.Passed {
			status = "🔴 FAIL"
			val = fmt.Sprintf("Failed at [%s]: %s", r.Stage, r.Err)
		}
		fields = append(fields, discordField{
			Name:   fmt.Sprintf("%s %s", status, r.Target),
			Value:  val,
			Inline: false,
		})
	}

	embed := discordEmbed{
		Title:       title,
		Description: truncate(summaryText),
		Color:       color,
		Fields:      fields,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	return discordPayload{
		Content: summaryText,
		Embeds:  []discordEmbed{embed},
	}
}

type genericPayload struct {
	Passed    bool            `json:"passed"`
	Total     int             `json:"total"`
	Failed    int             `json:"failed"`
	Hostname  string          `json:"hostname,omitempty"`
	Timestamp string          `json:"timestamp"`
	Results   []genericResult `json:"results"`
}

type genericResult struct {
	Target            string         `json:"target"`
	Tags              []string       `json:"tags,omitempty"`
	Passed            bool           `json:"passed"`
	Stage             string         `json:"stage"`
	Error             string         `json:"error,omitempty"`
	BackupPath        string         `json:"backup_path,omitempty"`
	BackupSize        int64          `json:"backup_size,omitempty"`
	BackupSizeHuman   string         `json:"backup_size_human,omitempty"`
	BackupAge         string         `json:"backup_age,omitempty"`
	DurationMs        int64          `json:"duration_ms"`
	RestoreDurationMs int64          `json:"restore_duration_ms,omitempty"`
	Checks            []genericCheck `json:"checks,omitempty"`
	DebugHint         string         `json:"debug_hint,omitempty"`
}

type genericCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Value  int64  `json:"value"`
	Reason string `json:"reason,omitempty"`
}

func buildGeneric(results []verify.Result) genericPayload {
	host, _ := os.Hostname()
	payload := genericPayload{
		Total:     len(results),
		Passed:    !anyFailed(results),
		Hostname:  host,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	for _, r := range results {
		gr := genericResult{
			Target:            r.Target,
			Tags:              r.Tags,
			Passed:            r.Passed,
			Stage:             string(r.Stage),
			DurationMs:        r.Duration.Milliseconds(),
			RestoreDurationMs: r.RestoreDuration.Milliseconds(),
			DebugHint:         r.DebugHint,
		}
		if r.Err != nil {
			gr.Error = r.Err.Error()
			payload.Failed++
		}
		if r.Backup != nil {
			gr.BackupPath = r.Backup.Path
			gr.BackupSize = r.Backup.Size
			gr.BackupSizeHuman = backup.HumanSize(r.Backup.Size)
			gr.BackupAge = r.Backup.Age(time.Now()).Round(time.Second).String()
		}
		for _, c := range r.Checks {
			gr.Checks = append(gr.Checks, genericCheck{
				Name:   c.Name,
				Passed: c.Passed,
				Value:  c.Value,
				Reason: c.Reason,
			})
		}
		payload.Results = append(payload.Results, gr)
	}
	return payload
}

type telegramPayload struct {
	ChatID    string `json:"chat_id,omitempty"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

func (n *Notifier) buildTelegram(results []verify.Result) telegramPayload {
	failed := anyFailed(results)
	header := "<b>✅ Lazarus: All Restorations Passed</b>"
	if failed {
		header = "<b>🚨 Lazarus: Restoration Drill Failed</b>"
	}
	summaryText := FormatMessage(results)
	escapedSummary := htmlEscape(summaryText)
	text := fmt.Sprintf("%s\n\n<pre>%s</pre>", header, truncate(escapedSummary))

	var chatID string
	if u, err := url.Parse(n.url); err == nil {
		chatID = u.Query().Get("chat_id")
	}

	return telegramPayload{
		ChatID:    chatID,
		Text:      text,
		ParseMode: "HTML",
	}
}

type teamsPayload struct {
	Type       string         `json:"@type"`
	Context    string         `json:"@context"`
	ThemeColor string         `json:"themeColor"`
	Summary    string         `json:"summary"`
	Title      string         `json:"title"`
	Sections   []teamsSection `json:"sections"`
}

type teamsSection struct {
	ActivityTitle string `json:"activityTitle,omitempty"`
	Text          string `json:"text"`
}

func buildTeams(results []verify.Result) teamsPayload {
	failed := anyFailed(results)
	themeColor := "2ecc71" // green
	title := "✅ Lazarus: All Restorations Passed"
	if failed {
		themeColor = "e74c3c" // red
		title = "🚨 Lazarus: Restoration Drill Failed"
	}
	summaryText := FormatMessage(results)
	return teamsPayload{
		Type:       "MessageCard",
		Context:    "http://schema.org/extensions",
		ThemeColor: themeColor,
		Summary:    title,
		Title:      title,
		Sections: []teamsSection{
			{
				ActivityTitle: "Restoration Drill Summary",
				Text:          fmt.Sprintf("```\n%s\n```", truncate(summaryText)),
			},
		},
	}
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// FormatMessage builds the human-readable summary sent to chat webhooks.
func FormatMessage(results []verify.Result) string {
	var b strings.Builder

	failed := failedResults(results)
	if len(failed) == 0 {
		fmt.Fprintf(&b, "✅ *Lazarus*: %d backup(s) verified restorable", len(results))
		for _, r := range results {
			fmt.Fprintf(&b, "\n• %s", r.Target)
		}
		return truncate(b.String())
	}

	fmt.Fprintf(&b, "🔴 *Lazarus*: %d of %d backup(s) failed verification", len(failed), len(results))
	for _, r := range failed {
		fmt.Fprintf(&b, "\n• *%s* failed at `%s`", r.Target, r.Stage)
		if r.Err != nil {
			fmt.Fprintf(&b, "\n  %s", firstLine(r.Err.Error()))
		}
		if r.Backup != nil {
			fmt.Fprintf(&b, "\n  backup: `%s`", r.Backup.Path)
		}
	}
	return truncate(b.String())
}

func failedResults(results []verify.Result) []verify.Result {
	var failed []verify.Result
	for _, r := range results {
		if !r.Passed {
			failed = append(failed, r)
		}
	}
	return failed
}

func anyFailed(results []verify.Result) bool {
	return len(failedResults(results)) > 0
}

// firstLine keeps a multi-line database error from dominating the message;
// the full text is still in the command's own output.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s)
}

func truncate(s string) string {
	if len(s) <= maxMessageLen {
		return s
	}

	// Back off to the nearest rune boundary so a multi-byte character
	// (Chinese text, an emoji) straddling the cutoff isn't split in half —
	// that would leave an invalid UTF-8 byte sequence in the JSON payload.
	cut := maxMessageLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "… (truncated)"
}

func (n *Notifier) sendEmail(ctx context.Context, results []verify.Result) error {
	if n.smtp == nil {
		return fmt.Errorf("smtp configuration is missing")
	}
	if n.smtp.Host == "" {
		return fmt.Errorf("smtp host is required")
	}
	if len(n.smtp.To) == 0 {
		return fmt.Errorf("smtp recipients (to) cannot be empty")
	}

	port := n.smtp.Port
	if port == 0 {
		port = 587
	}
	addr := fmt.Sprintf("%s:%d", n.smtp.Host, port)

	var auth smtp.Auth
	if n.smtp.Username != "" && n.smtp.Password != "" {
		auth = smtp.PlainAuth("", n.smtp.Username, n.smtp.Password, n.smtp.Host)
	}

	failed := anyFailed(results)
	subject := "[Lazarus] ✅ All Database Restorations Passed"
	if failed {
		failedCount := len(failedResults(results))
		subject = fmt.Sprintf("[Lazarus] 🚨 %d/%d Database Restorations FAILED", failedCount, len(results))
	}

	from := n.smtp.From
	if from == "" {
		from = "lazarus@localhost"
	}

	htmlBody := buildHTMLReport(results)

	var msg bytes.Buffer
	msg.WriteString(fmt.Sprintf("From: %s\r\n", from))
	msg.WriteString(fmt.Sprintf("To: %s\r\n", strings.Join(n.smtp.To, ", ")))
	msg.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	msg.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z)))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(htmlBody)

	sender := n.smtpSender
	if sender == nil {
		sender = smtp.SendMail
	}

	// Respect context timeout/cancellation
	done := make(chan error, 1)
	go func() {
		done <- sender(addr, auth, from, n.smtp.To, msg.Bytes())
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return fmt.Errorf("smtp send failed: %w", err)
		}
		return nil
	}
}

func buildHTMLReport(results []verify.Result) string {
	failed := anyFailed(results)
	total := len(results)
	failedCount := len(failedResults(results))
	passedCount := total - failedCount

	headerBg := "#10b981" // emerald green
	headerText := "All Database Restorations Passed"
	if failed {
		headerBg = "#ef4444" // red
		headerText = fmt.Sprintf("%d of %d Restorations Failed", failedCount, total)
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "lazarus"
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>Lazarus Drill Report</title></head>")
	b.WriteString("<body style=\"margin:0;padding:24px;background-color:#f8fafc;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;color:#1e293b;\">")
	b.WriteString("<div style=\"max-width:680px;margin:0 auto;background:#ffffff;border-radius:8px;overflow:hidden;box-shadow:0 2px 8px rgba(0,0,0,0.06);border:1px solid #e2e8f0;\">")

	// Header Banner
	fmt.Fprintf(&b, "<div style=\"background-color:%s;color:#ffffff;padding:24px 28px;\">", headerBg)
	fmt.Fprintf(&b, "<h1 style=\"margin:0 0 6px 0;font-size:20px;font-weight:700;\">%s</h1>", headerText)
	fmt.Fprintf(&b, "<p style=\"margin:0;font-size:13px;opacity:0.9;\">Host: %s &bull; Time: %s</p>", htmlEscape(host), time.Now().UTC().Format(time.RFC3339))
	b.WriteString("</div>")

	// Metric Cards
	b.WriteString("<div style=\"padding:20px 28px;border-bottom:1px solid #f1f5f9;background:#f8fafc;display:flex;gap:12px;\">")
	fmt.Fprintf(&b, "<span style=\"display:inline-block;padding:6px 12px;background:#e2e8f0;border-radius:6px;font-size:13px;font-weight:600;margin-right:8px;\">Total: %d</span>", total)
	fmt.Fprintf(&b, "<span style=\"display:inline-block;padding:6px 12px;background:#dcfce7;color:#166534;border-radius:6px;font-size:13px;font-weight:600;margin-right:8px;\">Passed: %d</span>", passedCount)
	if failedCount > 0 {
		fmt.Fprintf(&b, "<span style=\"display:inline-block;padding:6px 12px;background:#fee2e2;color:#991b1b;border-radius:6px;font-size:13px;font-weight:600;\">Failed: %d</span>", failedCount)
	}
	b.WriteString("</div>")

	// Target Items
	b.WriteString("<div style=\"padding:24px 28px;\">")
	for _, r := range results {
		borderColor := "#e2e8f0"
		badgeBg := "#dcfce7"
		badgeColor := "#166534"
		badgeText := "PASSED"
		if !r.Passed {
			borderColor = "#fecaca"
			badgeBg = "#fee2e2"
			badgeColor = "#991b1b"
			badgeText = "FAILED"
		}

		fmt.Fprintf(&b, "<div style=\"border:1px solid %s;border-radius:6px;padding:16px;margin-bottom:16px;\">", borderColor)
		b.WriteString("<div style=\"display:flex;align-items:center;margin-bottom:8px;\">")
		fmt.Fprintf(&b, "<strong style=\"font-size:15px;color:#0f172a;\">%s</strong>", htmlEscape(r.Target))
		fmt.Fprintf(&b, "<span style=\"margin-left:auto;background:%s;color:%s;font-size:11px;font-weight:700;padding:3px 8px;border-radius:4px;\">%s</span>", badgeBg, badgeColor, badgeText)
		b.WriteString("</div>")

		// Tags
		if len(r.Tags) > 0 {
			b.WriteString("<div style=\"margin-bottom:8px;\">")
			for _, tag := range r.Tags {
				fmt.Fprintf(&b, "<span style=\"background:#e0f2fe;color:#0369a1;font-size:11px;padding:2px 6px;border-radius:3px;margin-right:4px;\">#%s</span>", htmlEscape(tag))
			}
			b.WriteString("</div>")
		}

		// Details
		b.WriteString("<div style=\"font-size:13px;color:#475569;line-height:1.6;\">")
		if r.Backup != nil {
			fmt.Fprintf(&b, "<div>&bull; Backup: <code>%s</code> (%s, %s ago)</div>", htmlEscape(r.Backup.Path), backup.HumanSize(r.Backup.Size), r.Backup.Age(time.Now()).Round(time.Second).String())
		}
		fmt.Fprintf(&b, "<div>&bull; Restore Time: %s (Total: %s)</div>", r.RestoreDuration.Truncate(time.Millisecond), r.Duration.Truncate(time.Millisecond))
		if !r.Passed {
			fmt.Fprintf(&b, "<div style=\"color:#b91c1c;margin-top:4px;\">&bull; Stage: <strong>%s</strong> &mdash; %s</div>", htmlEscape(string(r.Stage)), htmlEscape(r.Err.Error()))
			if r.DebugHint != "" {
				fmt.Fprintf(&b, "<div style=\"background:#fff1f2;padding:6px 10px;border-radius:4px;color:#9f1239;font-size:12px;margin-top:4px;\">💡 Hint: %s</div>", htmlEscape(r.DebugHint))
			}
		}

		if len(r.Checks) > 0 {
			b.WriteString("<div style=\"margin-top:6px;font-size:12px;\">Checks: ")
			for _, c := range r.Checks {
				checkIcon := "✅"
				if !c.Passed {
					checkIcon = "❌"
				}
				fmt.Fprintf(&b, "<span style=\"margin-right:8px;\">%s %s</span>", checkIcon, htmlEscape(c.Name))
			}
			b.WriteString("</div>")
		}

		b.WriteString("</div>") // details
		b.WriteString("</div>") // card
	}
	b.WriteString("</div>") // target list

	// Footer
	b.WriteString("<div style=\"padding:16px 28px;background:#f8fafc;border-top:1px solid #f1f5f9;font-size:12px;color:#94a3b8;text-align:center;\">")
	b.WriteString("Generated by <a href=\"https://github.com/chen-y/lazarus\" style=\"color:#64748b;text-decoration:none;font-weight:600;\">Lazarus</a> Backup Verification Runner")
	b.WriteString("</div>")

	b.WriteString("</div></body></html>")
	return b.String()
}
