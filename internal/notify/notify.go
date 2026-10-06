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
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"lazarus/internal/backup"
	"lazarus/internal/verify"
)

// Format shapes the webhook body.
type Format string

const (
	FormatSlack    Format = "slack"
	FormatDiscord  Format = "discord"
	FormatTelegram Format = "telegram"
	FormatTeams    Format = "teams"
	FormatGeneric  Format = "generic"
	FormatLazarus  Format = "lazarus"
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
	url    string
	format Format
	when   When
	apiKey string
	client *http.Client
}

func New(url string, format Format, when When) *Notifier {
	return &Notifier{
		url:    url,
		format: format,
		when:   when,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// WithAPIKey attaches an authentication token/key to outgoing webhook requests.
func (n *Notifier) WithAPIKey(key string) *Notifier {
	if n != nil {
		n.apiKey = key
	}
	return n
}

// ShouldSend reports whether this run's outcome warrants a message.
func (n *Notifier) ShouldSend(results []verify.Result) bool {
	if n == nil || n.url == "" || n.when == WhenNever {
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
func (n *Notifier) Send(ctx context.Context, results []verify.Result) error {
	payload, err := n.buildPayload(results)
	if err != nil {
		return err
	}

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
	if err != nil {
		return fmt.Errorf("send webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) buildPayload(results []verify.Result) ([]byte, error) {
	switch n.format {
	case FormatDiscord:
		return json.Marshal(buildDiscord(results))
	case FormatTelegram:
		return json.Marshal(n.buildTelegram(results))
	case FormatTeams:
		return json.Marshal(buildTeams(results))
	case FormatGeneric, FormatLazarus:
		return json.Marshal(buildGeneric(results))
	default: // Slack, and anything Slack-compatible (Mattermost, etc.)
		return json.Marshal(buildSlack(results))
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
