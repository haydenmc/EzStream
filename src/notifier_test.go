package main

import (
	"testing"
	"time"
)

func TestSubscribeAndBroadcast(t *testing.T) {
	n := NewNotifier()
	_, ch := n.Subscribe()

	notification := ChannelNotification{Id: "c1", Name: "Test", IsLive: true}
	n.Broadcast(notification)

	select {
	case got := <-ch:
		if got != notification {
			t.Fatalf("expected %v, got %v", notification, got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

func TestUnsubscribe(t *testing.T) {
	n := NewNotifier()
	id, ch := n.Subscribe()
	n.Unsubscribe(id)

	// Channel should be closed after unsubscribe
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed")
	}
}

func TestBroadcastToMultipleSubscribers(t *testing.T) {
	n := NewNotifier()
	_, ch1 := n.Subscribe()
	_, ch2 := n.Subscribe()

	notification := ChannelNotification{Id: "c1", Name: "Test", IsLive: true}
	n.Broadcast(notification)

	for i, ch := range []<-chan ChannelNotification{ch1, ch2} {
		select {
		case got := <-ch:
			if got != notification {
				t.Fatalf("subscriber %d: expected %v, got %v", i, notification, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out", i)
		}
	}
}

func TestBroadcastDropsWhenFull(t *testing.T) {
	n := NewNotifier()
	_, ch := n.Subscribe()

	// Fill the buffer (capacity 1)
	n.Broadcast(ChannelNotification{Id: "c1", IsLive: true})
	// This should be dropped, not block
	n.Broadcast(ChannelNotification{Id: "c2", IsLive: true})

	got := <-ch
	if got.Id != "c1" {
		t.Fatalf("expected c1, got %s", got.Id)
	}

	// Channel should be empty now
	select {
	case <-ch:
		t.Fatal("expected no more notifications")
	default:
	}
}

func TestUnsubscribeIdempotent(t *testing.T) {
	n := NewNotifier()
	id, _ := n.Subscribe()
	n.Unsubscribe(id)
	// Should not panic
	n.Unsubscribe(id)
}

func TestUniqueIds(t *testing.T) {
	n := NewNotifier()
	id1, _ := n.Subscribe()
	id2, _ := n.Subscribe()
	if id1 == id2 {
		t.Fatal("expected unique IDs")
	}
}
