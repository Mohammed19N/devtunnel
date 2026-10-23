package inspector

import (
	"testing"
	"time"
)

func TestStoreRing(t *testing.T) {
	s := NewStore(3)
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
	s := NewStore(3)
	ch, cancel := s.Subscribe()
	id := s.Add(&Exchange{Request: Request{Method: "GET", URI: "/x"}})
	select {
	case sum := <-ch:
		if sum.ID != id || sum.Method != "GET" || sum.URI != "/x" {
			t.Fatalf("summary = %+v", sum)
		}
	case <-time.After(time.Second):
		t.Fatal("no summary")
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed")
	}
	s.Add(&Exchange{})
	cancel() // idempotent
}

func TestStoreSlowSubscriberDoesNotBlock(t *testing.T) {
	s := NewStore(10)
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
