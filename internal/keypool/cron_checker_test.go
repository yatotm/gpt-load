package keypool

import (
	"testing"
	"time"

	"gorm.io/datatypes"
	"gpt-load/internal/store"
	"gpt-load/internal/types"
)

func TestRecordProbeWindowRequiresCompleteInitialWindow(t *testing.T) {
	checker := &CronChecker{Store: store.NewMemoryStore()}
	start := time.Unix(1_700_000_000, 0)

	stats, err := checker.recordProbeWindow(1, 10, false, start)
	if err != nil {
		t.Fatalf("recordProbeWindow returned error: %v", err)
	}
	if stats.WindowComplete {
		t.Fatal("expected first sample to stay in warmup window")
	}

	stats, err = checker.recordProbeWindow(1, 10, false, start.Add(9*time.Minute))
	if err != nil {
		t.Fatalf("recordProbeWindow returned error: %v", err)
	}
	if stats.WindowComplete {
		t.Fatal("expected warmup window to remain incomplete before 10 minutes")
	}

	stats, err = checker.recordProbeWindow(1, 10, false, start.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("recordProbeWindow returned error: %v", err)
	}
	if !stats.WindowComplete {
		t.Fatal("expected window to become complete after the full first window")
	}
}

func TestDecideProbeStatusChangeSkipsWarmupWindow(t *testing.T) {
	decision := decideProbeStatusChange(probeWindowStats{
		SampleCount:    3,
		FailureRate:    100,
		WindowComplete: false,
	}, true, 10)

	if decision.ShouldBlacklist || decision.ShouldRestore {
		t.Fatal("expected warmup window to skip active blacklist/restore decisions")
	}
}

func TestHasKeySpecificActiveProbeConfig(t *testing.T) {
	if !hasKeySpecificActiveProbeConfig(datatypes.JSONMap{"active_probe_enabled": true}) {
		t.Fatal("expected active probe override to be detected")
	}
	if hasKeySpecificActiveProbeConfig(datatypes.JSONMap{"blacklist_threshold": 0}) {
		t.Fatal("expected unrelated key override not to count as active probe override")
	}
}

func TestShouldUseActiveProbeForKey(t *testing.T) {
	activeConfig := types.SystemSettings{ActiveProbeEnabled: true}
	inactiveConfig := types.SystemSettings{ActiveProbeEnabled: false}

	if shouldUseActiveProbeForKey(false, nil, inactiveConfig) {
		t.Fatal("expected inactive probe config to disable active probing")
	}
	if !shouldUseActiveProbeForKey(false, nil, activeConfig) {
		t.Fatal("expected active probing to run outside idle periods")
	}
	if shouldUseActiveProbeForKey(true, nil, activeConfig) {
		t.Fatal("expected idle periods to disable group-default active probing")
	}
	if !shouldUseActiveProbeForKey(true, datatypes.JSONMap{"active_probe_enabled": true}, activeConfig) {
		t.Fatal("expected key-level active probe override to bypass idle-period disable")
	}
}
