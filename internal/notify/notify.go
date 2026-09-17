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
	"strings"
	"time"
	"unicode/utf8"

	"lazarus/internal/verify"
)

// Format shapes the webhook body.
type Format string

const (
	FormatSlack   Format = "slack"
	FormatDiscord Format = "discord"
	FormatGeneric Format = "generic"
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
		return json.Marshal(map[string]string{"content": FormatMessage(results)})
	case FormatGeneric:
		return json.Marshal(buildGeneric(results))
	default: // Slack, and anything Slack-compatible (Mattermost, etc.)
		return json.Marshal(map[string]string{"text": FormatMessage(results)})
	}
}

type genericPayload struct {
	Passed  bool            `json:"passed"`
	Total   int             `json:"total"`
	Failed  int             `json:"failed"`
	Results []genericResult `json:"results"`
}

type genericResult struct {
	Target string `json:"target"`
	Passed bool   `json:"passed"`
	Stage  string `json:"stage"`
	Error  string `json:"error,omitempty"`
}

func buildGeneric(results []verify.Result) genericPayload {
	payload := genericPayload{Total: len(results), Passed: !anyFailed(results)}

	for _, r := range results {
		gr := genericResult{Target: r.Target, Passed: r.Passed, Stage: string(r.Stage)}
		if r.Err != nil {
			gr.Error = r.Err.Error()
			payload.Failed++
		}
		payload.Results = append(payload.Results, gr)
	}
	return payload
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
