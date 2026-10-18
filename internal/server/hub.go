package server

import (
	"sort"
	"sync"
)

const listTopic = "list"

type sub struct {
	ch    chan []byte
	email string
	name  string
}

// hub fans SSE frames out per topic: "order:<id>" for people on an order page,
// "list" for the running orders. A subscriber that falls behind loses frames
// instead of blocking everyone; the next snapshot carries the full state.
type hub struct {
	mu     sync.Mutex
	topics map[string]map[*sub]struct{}
}

func newHub() *hub { return &hub{topics: map[string]map[*sub]struct{}{}} }

func (h *hub) join(topic, email, name string) *sub {
	s := &sub{ch: make(chan []byte, 16), email: email, name: name}
	h.mu.Lock()
	if h.topics[topic] == nil {
		h.topics[topic] = map[*sub]struct{}{}
	}
	h.topics[topic][s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) leave(topic string, s *sub) {
	h.mu.Lock()
	delete(h.topics[topic], s)
	if len(h.topics[topic]) == 0 {
		delete(h.topics, topic)
	}
	h.mu.Unlock()
}

func (h *hub) publish(topic string, frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.topics[topic] {
		select {
		case s.ch <- frame:
		default:
		}
	}
}

// viewers lists the distinct people watching a topic.
func (h *hub) viewers(topic string) []string {
	h.mu.Lock()
	seen := map[string]string{}
	for s := range h.topics[topic] {
		if s.email != "" {
			seen[s.email] = s.name
		}
	}
	h.mu.Unlock()
	out := make([]string, 0, len(seen))
	for _, n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
