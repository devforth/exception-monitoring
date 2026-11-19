//go:build !unix

package ingest

import "time"

// cpuTime is not measured here.
func cpuTime() time.Duration { return 0 }
