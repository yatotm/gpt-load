package utils

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DailyTimeRange 表示一个按天循环的时间段，单位为分钟。
type DailyTimeRange struct {
	StartMinute int
	EndMinute   int
}

func (r DailyTimeRange) ContainsMinute(minute int) bool {
	if r.StartMinute < r.EndMinute {
		return minute >= r.StartMinute && minute < r.EndMinute
	}
	return minute >= r.StartMinute || minute < r.EndMinute
}

// ParseDailyTimeRanges 解析类似 "00:00-08:00,12:00-13:30" 的时段配置。
func ParseDailyTimeRanges(input string) ([]DailyTimeRange, error) {
	text := strings.TrimSpace(input)
	if text == "" {
		return nil, nil
	}

	parts := strings.Split(text, ",")
	ranges := make([]DailyTimeRange, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		bounds := strings.Split(part, "-")
		if len(bounds) != 2 {
			return nil, fmt.Errorf("invalid range %q, expected HH:MM-HH:MM", part)
		}

		startMinute, err := parseClockMinute(bounds[0])
		if err != nil {
			return nil, fmt.Errorf("invalid start time in %q: %w", part, err)
		}
		endMinute, err := parseClockMinute(bounds[1])
		if err != nil {
			return nil, fmt.Errorf("invalid end time in %q: %w", part, err)
		}
		if startMinute == endMinute {
			return nil, fmt.Errorf("range %q must not have the same start and end time", part)
		}

		ranges = append(ranges, DailyTimeRange{
			StartMinute: startMinute,
			EndMinute:   endMinute,
		})
	}

	if len(ranges) == 0 {
		return nil, nil
	}
	return ranges, nil
}

func IsWithinDailyTimeRanges(now time.Time, ranges []DailyTimeRange) bool {
	if len(ranges) == 0 {
		return false
	}

	minute := now.Hour()*60 + now.Minute()
	for _, item := range ranges {
		if item.ContainsMinute(minute) {
			return true
		}
	}
	return false
}

func parseClockMinute(input string) (int, error) {
	text := strings.TrimSpace(input)
	parts := strings.Split(text, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM")
	}

	hour, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("invalid hour")
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("invalid minute")
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("time out of range")
	}
	return hour*60 + minute, nil
}
