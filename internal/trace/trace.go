package trace

import (
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
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

type PendingEvent struct {
	Name           string `json:"name"` // name of the event, as displayed in Trace Viewer
	Categories     string `json:"cat"`  // event categories (comma-separated)
	Type           string `json:"ph"`   // event type (single character)
	ClockTimestamp uint64 `json:"ts"`   // tracing clock timestamp (microsecond granularity)
	Duration       uint64 `json:"dur"`
	Pid            uint64 `json:"pid"` // process ID for the process that output this event
	Tid            uint64 `json:"tid"` // thread ID for the thread that output this event

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

func Event(name string) *PendingEvent {
	return &PendingEvent{
		Name:           name,
		Type:           "X",
		ClockTimestamp: uint64(time.Since(start) / time.Microsecond),
		start:          time.Now(),
	}
}
