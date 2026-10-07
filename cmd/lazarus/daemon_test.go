package main

import (
	"testing"
	"time"

	"lazarus/internal/config"
)

func TestPlanDaemonPerTargetScheduling(t *testing.T) {
	now := time.Date(2026, 10, 7, 16, 30, 0, 0, time.Local)
	targets := []config.Target{
		{Name: "cron", Schedule: "0 4 * * *"},
		{Name: "interval", Interval: 6 * time.Hour},
		{Name: "default"},
	}
	plans, def := planDaemon(targets, 0, now)
	if def != time.Hour {
		t.Fatalf("default interval = %s, want 1h", def)
	}

	wantCron := time.Date(2026, 10, 8, 4, 0, 0, 0, time.Local)
	if !plans[0].next.Equal(wantCron) {
		t.Errorf("cron target first run = %s, want %s (waits for its slot)", plans[0].next, wantCron)
	}
	if !plans[1].next.Equal(now) || !plans[2].next.Equal(now) {
		t.Error("interval/default targets should run immediately")
	}
	if got := earliest(plans); !got.Equal(now) {
		t.Errorf("earliest = %s, want now", got)
	}

	plans[1].advance(now, def)
	plans[2].advance(now, def)
	plans[0].advance(wantCron, def)
	if !plans[1].next.Equal(now.Add(6 * time.Hour)) {
		t.Errorf("interval next = %s", plans[1].next)
	}
	if !plans[2].next.Equal(now.Add(time.Hour)) {
		t.Errorf("default next = %s", plans[2].next)
	}
	if !plans[0].next.Equal(wantCron.AddDate(0, 0, 1)) {
		t.Errorf("cron next = %s", plans[0].next)
	}
}

func TestPlanDaemonOverrideKeepsCron(t *testing.T) {
	now := time.Now()
	plans, _ := planDaemon([]config.Target{{Name: "c", Schedule: "@daily"}, {Name: "i", Interval: time.Hour}}, 5*time.Minute, now)
	if plans[0].cron == nil {
		t.Error("--interval must not override an explicit cron schedule")
	}
	if plans[1].interval != 5*time.Minute {
		t.Errorf("--interval should override target interval, got %s", plans[1].interval)
	}
}
