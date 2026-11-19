package background

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

// recorder keeps the order of the passes.
type recorder struct {
	mutex  sync.Mutex
	passes []string
}

func (record *recorder) job(name string, atStart bool, first, every time.Duration, took time.Duration) Job {
	return Job{Name: name, AtStart: atStart, First: first, Every: every, Run: func(context.Context) error {
		record.mutex.Lock()
		record.passes = append(record.passes, name+">")
		record.mutex.Unlock()
		time.Sleep(took)
		record.mutex.Lock()
		record.passes = append(record.passes, "<"+name)
		record.mutex.Unlock()

		return nil
	}}
}

func (record *recorder) list() []string {
	record.mutex.Lock()

	defer record.mutex.Unlock()

	return slices.Clone(record.passes)
}

// The jobs of a lane run at start in order, then on their schedule, never two at once.
func TestLaneRunsOneAtATime(t *testing.T) {
	record := &recorder{}
	wake := make(chan struct{}, 1)
	check := record.job("check", true, 80*time.Millisecond, 80*time.Millisecond, 30*time.Millisecond)
	check.Wake = wake
	lane := Lane{Name: "database", Jobs: []Job{
		record.job("compact", true, 40*time.Millisecond, 80*time.Millisecond, 30*time.Millisecond),
		check,
	}}
	stop := Start(context.Background(), slog.New(slog.DiscardHandler), lane)

	wake <- struct{}{}

	time.Sleep(300 * time.Millisecond)
	stop()

	passes := record.list()

	if len(passes) < 8 || !slices.Equal(passes[:4], []string{"compact>", "<compact", "check>", "<check"}) {
		t.Fatalf("passes %v", passes)
	}

	for index := 0; index+1 < len(passes); index += 2 {
		if passes[index][len(passes[index])-1] != '>' || passes[index+1][0] != '<' || passes[index][:len(passes[index])-1] != passes[index+1][1:] {
			t.Fatalf("two passes at once: %v", passes)
		}
	}

	if count := slices.Index(passes[2:], "check>"); count < 0 {
		t.Fatalf("the wake ran no pass: %v", passes)
	}
}

func TestDescribe(t *testing.T) {
	job := Job{Name: "size check", AtStart: true, First: 10 * time.Minute, Every: 10 * time.Minute, Note: "after 50 MB written"}

	if got := job.Describe(); got != "at start, at 10m0s then every 10m0s, after 50 MB written" {
		t.Errorf("%q", got)
	}
}

// A pass that ignores its time limit is left running: the lane goes on with
// the other jobs and skips the stuck one until it returns.
func TestStuckJobDoesNotStopTheLane(t *testing.T) {
	timeoutGrace = 10 * time.Millisecond

	t.Cleanup(func() { timeoutGrace = time.Second })

	release := make(chan struct{})
	record := &recorder{}
	stuck := Job{Name: "stuck", AtStart: true, First: 20 * time.Millisecond, Every: 20 * time.Millisecond, Timeout: 20 * time.Millisecond,
		Run: func(context.Context) error { <-release; return nil }}
	lane := Lane{Name: "database", Jobs: []Job{stuck, record.job("check", false, 30*time.Millisecond, 30*time.Millisecond, 0)}}
	stop := Start(context.Background(), slog.New(slog.DiscardHandler), lane)

	time.Sleep(250 * time.Millisecond)
	close(release)
	stop()

	if passes := len(record.list()) / 2; passes < 4 {
		t.Fatalf("the lane waited for the stuck job: %d passes of the other", passes)
	}
}

// A pass that stops at its time limit (a resumable cleanup) goes on soon, not at its next schedule.
func TestJobAtItsLimitGoesOnSoon(t *testing.T) {
	var passes sync.WaitGroup

	passes.Add(3)

	count := 0
	slow := Job{Name: "cleanup", AtStart: true, First: time.Hour, Every: time.Hour, Timeout: 10 * time.Millisecond,
		Run: func(ctx context.Context) error {
			if count++; count <= 3 {
				defer passes.Done()
			}

			<-ctx.Done()

			return ctx.Err()
		}}
	stop := Start(context.Background(), slog.New(slog.DiscardHandler), Lane{Name: "database", Jobs: []Job{slow}})
	finished := make(chan struct{})

	go func() { passes.Wait(); close(finished) }()

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Error("a pass stopped at its limit waited for its schedule")
	}

	stop()
}
