package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// https://docs.google.com/document/d/1CvAClvFfyA5R-PhYUmn5OOQtYMH4h6I0nSsKchNAySU/edit

var start = time.Now()

var (
	sinkMu sync.Mutex
	sink   io.Writer = ioutil.Discard
)

// Sink writes all following Event()s as a Chrome trace event file into w.
func Sink(w io.Writer) {
	sinkMu.Lock()
	defer sinkMu.Unlock()
	sink = w
	// Start the JSON Array Format
	w.Write([]byte{'['})
	// The ] at the end is optional, so we skip it
}

// Enable is a convenience function for creating a file in
// $TMPDIR/distri.traces/prefix.$PID.
//
// The filename assumes the OS does not frequently re-use the same pid.
func Enable(prefix string) error {
	fn := filepath.Join(os.TempDir(), "distri.traces", fmt.Sprintf("%s.%d", prefix, os.Getpid()))
	if err := os.MkdirAll(filepath.Dir(fn), 0755); err != nil {
		return err
	}
	f, err := os.Create(fn)
	if err != nil {
		return err
	}
	Sink(f)
	return nil
}

func parseIntOr0(s string) uint64 {
	n, _ := strconv.ParseUint(s, 0, 64)
	return n
}

func cpuEvents(last map[string]map[string]uint64) error {
	b, err := ioutil.ReadFile("/proc/stat")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(line, "cpu") ||
			strings.HasPrefix(line, "cpu ") {
			continue
		}
		// cpu   user   nice sys idle    …
		// cpu10 126780 18 25115 1757702 300 1255 357 0 0 0
		parts := strings.Split(line, " ")
		if len(parts) < 5 {
			continue
		}
		lm, ok := last[parts[0]]
		if !ok {
			lm = make(map[string]uint64)
			last[parts[0]] = lm
		}
		ev := Event(parts[0], 0)
		ev.Pid = 2
		ev.Type = "C" // counter
		_, present := lm["user"]
		user := parseIntOr0(parts[1])
		userDiff := user - lm["user"]
		lm["user"] = user

		sys := parseIntOr0(parts[3])
		sysDiff := sys - lm["sys"]
		lm["sys"] = sys

		if !present {
			continue
		}
		ev.Args = map[string]uint64{
			"user": userDiff,
			"sys":  sysDiff,
		}
		ev.Done()
	}
	return nil
}

func CPUEvents(ctx context.Context, frequency time.Duration) error {
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	last := make(map[string]map[string]uint64)
	cpuEvents(last) // initialize
	cpuEvents(last) // print 0 values immediately
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := memEvents(); err != nil {
				return fmt.Errorf("memEvents: %v", err)
			}
			if err := cpuEvents(last); err != nil {
				return fmt.Errorf("cpuEvents: %v", err)
			}
		}
	}
}

func memEvents() error {
	b, err := ioutil.ReadFile("/proc/meminfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, "MemAvailable:"))
		kb, err := strconv.ParseUint(strings.TrimSuffix(val, " kB"), 0, 64)
		if err != nil {
			return err
		}
		ev := Event("MemAvailable", 0)
		ev.Pid = 1
		ev.Type = "C" // counter
		ev.Args = map[string]uint64{"available": kb}
		ev.Done()
		break
	}
	return nil
}

func MemEvents(ctx context.Context, frequency time.Duration) error {
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := memEvents(); err != nil {
				return fmt.Errorf("memEvents: %v", err)
			}
		}
	}
}

type PendingEvent struct {
	Name           string      `json:"name"` // name of the event, as displayed in Trace Viewer
	Categories     string      `json:"cat"`  // event categories (comma-separated)
	Type           string      `json:"ph"`   // event type (single character)
	ClockTimestamp uint64      `json:"ts"`   // tracing clock timestamp (microsecond granularity)
	Duration       uint64      `json:"dur"`
	Pid            uint64      `json:"pid"` // process ID for the process that output this event
	Tid            uint64      `json:"tid"` // thread ID for the thread that output this event
	Args           interface{} `json:"args"`

	start time.Time
}

func (pe *PendingEvent) Done() {
	pe.Duration = uint64(time.Since(pe.start) / time.Microsecond)
	b, err := json.Marshal(pe)
	if err != nil {
		panic(err)
	}
	sinkMu.Lock()
	defer sinkMu.Unlock()
	if _, err := sink.Write(append(b, ',')); err != nil {
		log.Printf("[trace] %v", err)
	}
}

func Event(name string, tid int) *PendingEvent {
	return &PendingEvent{
		Name:           name,
		Type:           "X",
		ClockTimestamp: uint64(time.Since(start) / time.Microsecond),
		Tid:            uint64(tid),
		start:          time.Now(),
	}
}
