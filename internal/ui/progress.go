package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"lazarus/internal/config"
	"lazarus/internal/verify"
)

// TargetProgress tracks live execution status of an individual target drill.
type TargetProgress struct {
	Name      string
	Engine    string
	Stage     verify.Stage
	Detail    string
	StartTime time.Time
	Duration  time.Duration
	Done      bool
	Passed    bool
}

// Tracker coordinates live multi-line terminal progress for parallel drills.
type Tracker struct {
	w       io.Writer
	mu      sync.Mutex
	targets []*TargetProgress
	indices map[string]int
	started time.Time
	ticker  *time.Ticker
	done    chan struct{}
	enabled bool
	lines   int
}

// NewTracker creates a live multi-target progress bar tracker.
// If enabled is false (e.g. non-TTY, CI redirect, or --no-live), it operates as a no-op.
func NewTracker(w io.Writer, targets []config.Target, enabled bool) *Tracker {
	tpList := make([]*TargetProgress, len(targets))
	idxMap := make(map[string]int, len(targets))

	now := time.Now()
	for i, t := range targets {
		tpList[i] = &TargetProgress{
			Name:      t.Name,
			Engine:    string(t.Engine),
			Stage:     verify.StagePreHook,
			StartTime: now,
		}
		idxMap[t.Name] = i
	}

	return &Tracker{
		w:       w,
		targets: tpList,
		indices: idxMap,
		started: now,
		done:    make(chan struct{}),
		enabled: enabled,
	}
}

// Start begins the live redraw loop.
func (t *Tracker) Start() {
	if !t.enabled || len(t.targets) == 0 {
		return
	}

	t.render(true)
	t.ticker = time.NewTicker(100 * time.Millisecond)

	go func() {
		for {
			select {
			case <-t.done:
				return
			case <-t.ticker.C:
				t.mu.Lock()
				t.render(false)
				t.mu.Unlock()
			}
		}
	}()
}

// Update updates the stage and state for a target.
func (t *Tracker) Update(target string, stage verify.Stage, detail string, done bool, passed bool) {
	if !t.enabled {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	idx, ok := t.indices[target]
	if !ok {
		return
	}

	tp := t.targets[idx]
	tp.Stage = stage
	tp.Detail = detail
	tp.Done = done
	tp.Passed = passed
	if done && tp.Duration == 0 {
		tp.Duration = time.Since(tp.StartTime)
	}
}

// Stop terminates the ticker and redraws the final completed progress status.
func (t *Tracker) Stop() {
	if !t.enabled {
		return
	}

	if t.ticker != nil {
		t.ticker.Stop()
		close(t.done)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.render(false)
	fmt.Fprintln(t.w)
}

func stagePercentage(s verify.Stage) int {
	switch s {
	case verify.StagePreHook:
		return 10
	case verify.StageFetch:
		return 20
	case verify.StageLocate:
		return 35
	case verify.StageSandbox:
		return 50
	case verify.StageRestore:
		return 75
	case verify.StageSchema:
		return 85
	case verify.StageChecks:
		return 92
	case verify.StagePostHook:
		return 96
	case verify.StageDone:
		return 100
	default:
		return 5
	}
}

func progressBar(pct int, width int) string {
	if width <= 0 {
		width = 12
	}
	filled := (pct * width) / 100
	if filled > width {
		filled = width
	}
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < filled; i++ {
		sb.WriteString("=")
	}
	if filled < width {
		sb.WriteString(">")
		for i := filled + 1; i < width; i++ {
			sb.WriteString(" ")
		}
	}
	sb.WriteString("]")
	return sb.String()
}

func (t *Tracker) render(first bool) {
	totalLines := len(t.targets) + 2

	if !first && t.lines > 0 {
		// Move cursor up to overwrite previous render
		fmt.Fprintf(t.w, "\033[%dA", t.lines)
	}

	completedCount := 0
	passedCount := 0

	for i, tp := range t.targets {
		// Clear current line
		fmt.Fprint(t.w, "\033[2K")

		elapsed := time.Since(tp.StartTime)
		if tp.Done && tp.Duration > 0 {
			elapsed = tp.Duration
		}

		pct := stagePercentage(tp.Stage)
		if tp.Done {
			pct = 100
			completedCount++
			if tp.Passed {
				passedCount++
			}
		}

		bar := progressBar(pct, 12)

		status := fmt.Sprintf("%-8s (%.1fs)", strings.ToUpper(string(tp.Stage)), elapsed.Seconds())
		if tp.Done {
			if tp.Passed {
				status = fmt.Sprintf("✓ PASS (%.1fs)", elapsed.Seconds())
			} else {
				status = fmt.Sprintf("✗ FAIL (%.1fs)", elapsed.Seconds())
			}
		}

		fmt.Fprintf(t.w, "  [%d/%d] %-20s [%-8s] %s %s\n",
			i+1, len(t.targets),
			truncateOrPad(tp.Name, 20),
			truncateOrPad(tp.Engine, 8),
			bar,
			status,
		)
	}

	// Status footer line
	fmt.Fprint(t.w, "\033[2K")
	overallElapsed := time.Since(t.started).Round(100 * time.Millisecond)
	fmt.Fprintf(t.w, "  ⏳ Elapsed: %s • Overall: %d/%d completed (%d passed)\n",
		overallElapsed, completedCount, len(t.targets), passedCount)

	t.lines = totalLines
}

func truncateOrPad(s string, width int) string {
	if len(s) > width {
		return s[:width]
	}
	return s
}

// IsTerminal returns true if f is an interactive TTY terminal.
func IsTerminal(f *os.File) bool {
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}
