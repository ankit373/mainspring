package server

import (
	"testing"

	"github.com/ankit373/mainspring/internal/cache"
)

func TestFlightGroupLeaderFollower(t *testing.T) {
	g := newFlightGroup()

	leader, f := g.join("k")
	if !leader || f != nil {
		t.Fatal("first caller must be the leader")
	}
	follower, ff := g.join("k")
	if follower || ff == nil {
		t.Fatal("second caller must be a follower with a flight to wait on")
	}

	// Follower unblocks with the published value.
	got := make(chan cache.Value, 1)
	go func() {
		<-ff.done
		got <- ff.val
	}()
	g.publish("k", cache.Value{Status: 200, Body: []byte("hi")}, true)

	v := <-got
	if v.Status != 200 || string(v.Body) != "hi" || !ff.ok {
		t.Fatalf("follower got %+v ok=%v, want the published value", v, ff.ok)
	}

	// After publish the key is cleared: a new caller is a fresh leader.
	leader2, _ := g.join("k")
	if !leader2 {
		t.Fatal("after publish, the key should be free for a new leader")
	}
}
