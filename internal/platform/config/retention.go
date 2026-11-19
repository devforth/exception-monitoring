package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Retention: how much data is kept. The database stays under
// MaxDatabaseBytes: the cleanup (package database) deletes the least useful data
// first when it nears it.
type Retention struct {
	MaxDatabaseBytes int64 // 0: no limit
}

const (
	// LogsPerMessage caps a message logged over and over (the same text
	// with other numbers or IDs) as it is written: its newest copies stay.
	LogsPerMessage = 10

	// The cleanup (database/cleanup.go; order in docs/configuration.md).
	CleanupStartPercent = 100
	CleanupStopPercent  = 80
	CleanupAge          = 30 * 24 * time.Hour
	CleanupRecentAge    = 7 * 24 * time.Hour
	OldEventsPerIssue   = 20
)

// DefaultMaxDatabaseBytes is the default of MAX_DATABASE_SIZE.
const DefaultMaxDatabaseBytes = 2 << 30 // 2 GB

// InboxPercent of MAX_DATABASE_SIZE (or of its default when it is off) may
// wait in the ingest queue; beyond it new envelopes get 429.
const InboxPercent = 20

// MaxInboxBytes is the ingest queue's share of maxDatabaseBytes (0: no limit).
func MaxInboxBytes(maxDatabaseBytes int64) int64 {
	if maxDatabaseBytes <= 0 {
		maxDatabaseBytes = DefaultMaxDatabaseBytes
	}

	return maxDatabaseBytes * InboxPercent / 100
}

func parseRetention(getenv func(string) string) (Retention, error) {
	retention := Retention{MaxDatabaseBytes: DefaultMaxDatabaseBytes}

	if value := strings.TrimSpace(getenv("MAX_DATABASE_SIZE")); value != "" {
		size, err := parseSize(value)

		if err != nil {
			return retention, fmt.Errorf("MAX_DATABASE_SIZE: %w", err)
		}

		retention.MaxDatabaseBytes = size
	}

	return retention, nil
}

// HumanDuration writes a duration for people, in its largest whole unit:
// "15 minutes", "1 hour", "30 days" (or as Go writes it when none fits).
func HumanDuration(duration time.Duration) string {
	for _, unit := range []struct {
		size time.Duration
		name string
	}{{24 * time.Hour, "day"}, {time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}} {
		if duration <= 0 || duration%unit.size != 0 {
			continue
		}

		count := int(duration / unit.size)

		if count == 1 {
			return "1 " + unit.name
		}

		return strconv.Itoa(count) + " " + unit.name + "s"
	}

	return duration.String()
}

var sizeUnits = []struct {
	suffix string
	bytes  int64
}{
	{"TB", 1 << 40},
	{"GB", 1 << 30},
	{"MB", 1 << 20},
	{"KB", 1 << 10},
	{"B", 1},
}

// parseSize reads "500MB", "2GB" or "1.5GB" (binary units: 1 GB = 1024 MB);
// "0" means no limit.
func parseSize(value string) (int64, error) {
	if value == "0" {
		return 0, nil
	}

	upper := strings.ToUpper(strings.ReplaceAll(value, " ", ""))

	for _, unit := range sizeUnits {
		if number, ok := strings.CutSuffix(upper, unit.suffix); ok {
			amount, err := strconv.ParseFloat(number, 64)

			if err != nil || amount <= 0 {
				break
			}

			return int64(amount * float64(unit.bytes)), nil
		}
	}

	return 0, fmt.Errorf("invalid size %q (e.g. 500MB, 2GB, or 0 for no limit)", value)
}

// FormatSize shows a byte count the way MAX_DATABASE_SIZE is written.
func FormatSize(bytes int64) string {
	for _, unit := range sizeUnits {
		if bytes >= unit.bytes && unit.bytes > 1 {
			return strconv.FormatFloat(float64(bytes)/float64(unit.bytes), 'f', 1, 64) + " " + unit.suffix
		}
	}

	return strconv.FormatInt(bytes, 10) + " B"
}
