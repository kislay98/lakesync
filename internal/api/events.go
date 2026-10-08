package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"lakesync/internal/store"
)

// hub fans events from the store's LISTEN channel out to SSE subscribers.
type hub struct {
	mu   sync.Mutex
	subs map[chan store.Event]struct{}
	log  *slog.Logger
}

func newHub(log *slog.Logger) *hub { return &hub{subs: map[chan store.Event]struct{}{}, log: log} }

func (h *hub) publish(e store.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // slow client: drop; the UI refetches on reconnect
		}
	}
}

func (h *hub) subscribe() chan store.Event {
	ch := make(chan store.Event, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan store.Event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}

// events streams Server-Sent Events. Each message has `event: <type>` and a
// JSON `data` payload. A comment line is sent every 20 s as a keep-alive.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "internal", "Streaming is not supported.", "")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprintf(w, "event: hello\ndata: %s\n\n", `{"ok":true}`)
	flusher.Flush()
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	keep := time.NewTicker(20 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keep.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b)
			flusher.Flush()
		}
	}
}
