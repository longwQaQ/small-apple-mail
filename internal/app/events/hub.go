package events

import (
	"encoding/json"
	"sync"
	"time"
)

type Event struct {
	ID   int64           `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	At   time.Time       `json:"at"`
}

type Hub struct {
	mu      sync.RWMutex
	nextID  int64
	clients map[chan Event]struct{}
	history []Event
}

func New() *Hub { return &Hub{clients: make(map[chan Event]struct{})} }

func (h *Hub) Publish(kind string, value any) Event {
	data, _ := json.Marshal(value)
	h.mu.Lock()
	h.nextID++
	e := Event{ID: h.nextID, Type: kind, Data: data, At: time.Now().UTC()}
	if len(h.history) == 500 {
		copy(h.history, h.history[1:])
		h.history[len(h.history)-1] = e
	} else {
		h.history = append(h.history, e)
	}
	for ch := range h.clients {
		select {
		case ch <- e:
		default:
		}
	}
	h.mu.Unlock()
	return e
}

func (h *Hub) Subscribe(after int64) (chan Event, func()) {
	// 缓冲区必须覆盖完整历史窗口，避免重连时在持锁状态下写满并阻塞。
	ch := make(chan Event, 512)
	h.mu.Lock()
	// 首次订阅只接收新事件；携带游标的重连才补发历史。
	if after > 0 {
		for _, e := range h.history {
			if e.ID > after {
				ch <- e
			}
		}
	}
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
