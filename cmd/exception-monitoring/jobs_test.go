package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/storage/storetest"
)

// Over a day no two jobs, of any lane, start in the same minute.
func TestJobsStartApart(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	notifier := notify.New(notify.Options{Store: storetest.Open(t), Logger: logger})

	t.Cleanup(func() { _ = notifier.Close(context.Background()) })

	starts := map[time.Duration]string{}

	for _, lane := range lanes(storetest.Open(t), notifier, 0, logger) {
		for _, job := range lane.Jobs {
			for at := job.First; at < 24*time.Hour; at += job.Every {
				minute := at.Truncate(time.Minute)

				if other, taken := starts[minute]; taken {
					t.Fatalf("%s and %s both start at %s", other, job.Name, at)
				}

				starts[minute] = job.Name
			}
		}
	}
}
