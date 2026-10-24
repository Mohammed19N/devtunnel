package inspector

import (
	"testing"
	"time"
)

func TestStoreRing(t *testing.T) {
	s := NewStore(3, DefaultMaxTotalBytes)
	for i := 0; i < 5; i++ {
		s.Add(&Exchange{})
	}
	l := s.List()
	if len(l) != 3 || l[0].ID != 5 || l[1].ID != 4 || l[2].ID != 3 {
		t.Fatalf("list = %+v", l)
	}
	if _, ok := s.Get(1); ok {
		t.Fatal("Get(1) should be evicted")
	}
	if e, ok := s.Get(4); !ok || e.ID != 4 {
		t.Fatal("Get(4) missing")
	}
	s.Clear()
	if len(s.List()) != 0 {
		t.Fatal("clear failed")
	}
}

func TestStoreSubscribe(t *testing.T) {
	s := NewStore(3, DefaultMaxTotalBytes)
	ch, cancel := s.Subscribe()
	id := s.Add(&Exchange{Request: Request{Method: "GET", URI: "/x"}})
	select {
	case ev := <-ch:
		if sum := ev.Summary; ev.Clear || sum.ID != id || sum.Method != "GET" || sum.URI != "/x" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no summary")
	}
	s.Clear()
	select {
	case ev := <-ch:
		if !ev.Clear {
			t.Fatalf("event = %+v, want clear", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no clear event")
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed")
	}
	s.Add(&Exchange{})
	cancel() // idempotent
}

func TestStoreSlowSubscriberDoesNotBlock(t *testing.T) {
	s := NewStore(10, DefaultMaxTotalBytes)
	_, cancel := s.Subscribe()
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			s.Add(&Exchange{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Add blocked")
	}
}

func TestStoreCapacity(t *testing.T) {
	if c := NewStore(0, DefaultMaxTotalBytes).Capacity(); c != DefaultCapacity {
		t.Fatalf("default capacity = %d", c)
	}
	if c := NewStore(7, DefaultMaxTotalBytes).Capacity(); c != 7 {
		t.Fatalf("capacity = %d", c)
	}
}

func bigExchange(n int) *Exchange {
	return &Exchange{Request: Request{Body: Body{Data: make([]byte, n)}}}
}

func TestStoreEvictsByBytes(t *testing.T) {
	s := NewStore(0, 3<<20)
	var first int64
	for i := 0; i < 4; i++ {
		id := s.Add(bigExchange(1 << 20))
		if i == 0 {
			first = id
		}
	}
	if _, ok := s.Get(first); ok {
		t.Fatal("first exchange should have been evicted")
	}
	if n := len(s.List()); n != 3 {
		t.Fatalf("List = %d, want 3", n)
	}
}

func TestStoreSingleHugeExchangeKept(t *testing.T) {
	s := NewStore(0, 1<<20)
	s.Add(bigExchange(1 << 10))
	id := s.Add(bigExchange(2 << 20))
	if _, ok := s.Get(id); !ok {
		t.Fatal("huge exchange must be kept")
	}
	if n := len(s.List()); n != 1 {
		t.Fatalf("List = %d, want 1", n)
	}
}
