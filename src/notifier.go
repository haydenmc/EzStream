package main

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

type Notifier struct {
	nextId      atomic.Uint64
	mu          sync.RWMutex
	subscribers map[uint64]chan ChannelNotification
}

func NewNotifier() *Notifier {
	return &Notifier{
		subscribers: map[uint64]chan ChannelNotification{},
	}
}

func (n *Notifier) Subscribe() (uint64, <-chan ChannelNotification) {
	id := n.nextId.Add(1)
	ch := make(chan ChannelNotification, 1)
	n.mu.Lock()
	n.subscribers[id] = ch
	n.mu.Unlock()
	return id, ch
}

func (n *Notifier) Unsubscribe(id uint64) {
	n.mu.Lock()
	if ch, ok := n.subscribers[id]; ok {
		delete(n.subscribers, id)
		close(ch)
	}
	n.mu.Unlock()
}

func (n *Notifier) Broadcast(notification ChannelNotification) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, ch := range n.subscribers {
		select {
		case ch <- notification:
		default:
			slog.Warn("Notifier: Dropped notification for slow subscriber")
		}
	}
}
