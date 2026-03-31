package utils

import (
	"testing"
	"time"
)

func TestParseDailyTimeRanges(t *testing.T) {
	ranges, err := ParseDailyTimeRanges("00:00-08:00, 23:00-06:00")
	if err != nil {
		t.Fatalf("ParseDailyTimeRanges returned error: %v", err)
	}
	if len(ranges) != 2 {
		t.Fatalf("expected 2 ranges, got %d", len(ranges))
	}
	if ranges[0].StartMinute != 0 || ranges[0].EndMinute != 8*60 {
		t.Fatalf("unexpected first range: %+v", ranges[0])
	}
	if ranges[1].StartMinute != 23*60 || ranges[1].EndMinute != 6*60 {
		t.Fatalf("unexpected second range: %+v", ranges[1])
	}
}

func TestParseDailyTimeRangesRejectsInvalidInput(t *testing.T) {
	if _, err := ParseDailyTimeRanges("08:00-08:00"); err == nil {
		t.Fatal("expected error for same start and end time")
	}
	if _, err := ParseDailyTimeRanges("25:00-26:00"); err == nil {
		t.Fatal("expected error for invalid hour")
	}
}

func TestIsWithinDailyTimeRanges(t *testing.T) {
	ranges, err := ParseDailyTimeRanges("00:00-08:00,23:00-06:00")
	if err != nil {
		t.Fatalf("ParseDailyTimeRanges returned error: %v", err)
	}

	if !IsWithinDailyTimeRanges(time.Date(2026, 3, 31, 7, 30, 0, 0, time.Local), ranges) {
		t.Fatal("expected 07:30 to be within idle ranges")
	}
	if !IsWithinDailyTimeRanges(time.Date(2026, 3, 31, 23, 30, 0, 0, time.Local), ranges) {
		t.Fatal("expected 23:30 to be within cross-midnight idle range")
	}
	if IsWithinDailyTimeRanges(time.Date(2026, 3, 31, 12, 0, 0, 0, time.Local), ranges) {
		t.Fatal("expected 12:00 to be outside idle ranges")
	}
}
