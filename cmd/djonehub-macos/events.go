package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// The call poll runs every few seconds, which is fine for showing state but far
// too slow to ring a phone: by the time a poll notices an incoming call, several
// seconds of it are gone. The module already announces calls the instant they
// happen through RING, +CLIP, CONNECT and NO CARRIER URCs, so those are
// forwarded straight to the browser over Server-Sent Events and the poll stays
// as the thing that keeps state honest.
type callEvent struct {
	Type   string    `json:"type"`             // ring | caller | connected | ended | state
	Number string    `json:"number,omitempty"` // present on caller, and on state when known
	State  string    `json:"state,omitempty"`  // present on state
	At     time.Time `json:"at"`
}

// eventHub fans one event out to every connected browser. Sends never block: a
// client that cannot keep up misses events rather than stalling the modem's URC
// handler, and the poll-driven "state" events bring it back in sync anyway.
type eventHub struct {
	mu      sync.Mutex
	clients map[chan callEvent]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{clients: make(map[chan callEvent]struct{})}
}

func (h *eventHub) subscribe() (<-chan callEvent, func()) {
	ch := make(chan callEvent, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

func (h *eventHub) publish(event callEvent) {
	if event.At.IsZero() {
		event.At = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

func (a *app) publishEvent(event callEvent) {
	if a.events != nil {
		a.events.publish(event)
	}
}

// wireCallURCs forwards the module's call URCs to connected browsers.
func (a *app) wireCallURCs() {
	if a.modem == nil || a.events == nil {
		return
	}
	a.modem.SetRingCallback(func() {
		log.Printf("incoming call: RING")
		a.publishEvent(callEvent{Type: "ring"})
	})
	a.modem.SetClipCallback(func(number string) {
		log.Printf("incoming call: +CLIP number=%q", number)
		a.publishEvent(callEvent{Type: "caller", Number: number})
	})
	a.modem.SetConnectCallback(func() {
		a.publishEvent(callEvent{Type: "connected"})
	})
	a.modem.SetHangupCallback(func() {
		a.publishEvent(callEvent{Type: "ended"})
	})
}

// eventsAPI streams call events to the browser.
func (a *app) eventsAPI(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "该连接不支持事件流")
		return
	}
	if a.events == nil {
		writeError(w, http.StatusServiceUnavailable, "事件流不可用")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Without this a reverse proxy may hold the stream in a buffer forever.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	stream, cancel := a.events.subscribe()
	defer cancel()

	// A periodic comment keeps intermediaries from closing an idle stream and
	// lets the browser notice a dead connection and reconnect.
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	encoder := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-stream:
			if !ok {
				return
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if err := encoder.Encode(event); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
