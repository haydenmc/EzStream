package main

import "testing"

func testChannelStore() *ChannelStore {
	return NewChannelStore([]ChannelInfo{
		{Id: "chan1", Name: "Channel One", AuthKey: "key1"},
		{Id: "chan2", Name: "Channel Two", AuthKey: "key2"},
	})
}

func TestFindById(t *testing.T) {
	store := testChannelStore()

	ch, err := store.FindById("chan1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ch.Name != "Channel One" {
		t.Fatalf("expected Channel One, got %s", ch.Name)
	}
}

func TestFindById_NotFound(t *testing.T) {
	store := testChannelStore()

	_, err := store.FindById("nonexistent")
	if err == nil {
		t.Fatal("expected error for missing channel")
	}
}

func TestFindByAuthKey(t *testing.T) {
	store := testChannelStore()

	ch, err := store.FindByAuthKey("key2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ch.Id != "chan2" {
		t.Fatalf("expected chan2, got %s", ch.Id)
	}
}

func TestFindByAuthKey_NotFound(t *testing.T) {
	store := testChannelStore()

	_, err := store.FindByAuthKey("badkey")
	if err == nil {
		t.Fatal("expected error for missing auth key")
	}
}

func TestAll(t *testing.T) {
	store := testChannelStore()

	all := store.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(all))
	}
}

func TestEmptyStore(t *testing.T) {
	store := NewChannelStore(nil)

	if len(store.All()) != 0 {
		t.Fatal("expected empty store")
	}

	_, err := store.FindById("any")
	if err == nil {
		t.Fatal("expected error on empty store")
	}
}
