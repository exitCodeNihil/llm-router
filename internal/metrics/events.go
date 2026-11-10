package metrics

import (
	"strings"
	"sync"
	"time"
)

const eventsRingSize = 50

// Event is one cluster-events entry (config epoch, version skew, SLO breach).
type Event struct {
	TS    time.Time `json:"ts"`
	Text  string    `json:"text"`
	Level string    `json:"level"` // info | warn | err
}

// Events is a small in-memory ring of cluster events.
// ponytail: per-process ring, lost on restart.
type Events struct {
	mu   sync.Mutex
	ring []Event
}

func (e *Events) Add(level, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ring = append(e.ring, Event{TS: time.Now(), Text: text, Level: level})
	if len(e.ring) > eventsRingSize {
		e.ring = e.ring[len(e.ring)-eventsRingSize:]
	}
}

// AddDedup appends only if the newest entry has different text — keeps
// read-time synthesized events (skew, SLO breach) from spamming the ring.
func (e *Events) AddDedup(level, text string) {
	e.mu.Lock()
	if n := len(e.ring); n > 0 && e.ring[n-1].Text == text {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	e.Add(level, text)
}

// Replace adds an event in place of the newest one when that one starts with
// prefix, so a run of the same kind of event (one config epoch after another)
// shows as its latest and doesn't push everything else out of the ring.
func (e *Events) Replace(prefix, level, text string) {
	e.mu.Lock()
	if n := len(e.ring); n > 0 && strings.HasPrefix(e.ring[n-1].Text, prefix) {
		e.ring[n-1] = Event{TS: time.Now(), Text: text, Level: level}
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	e.Add(level, text)
}

// Snapshot returns events newest-first.
func (e *Events) Snapshot() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, len(e.ring))
	for i, ev := range e.ring {
		out[len(e.ring)-1-i] = ev
	}
	return out
}
