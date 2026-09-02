package taskqueue

import (
	"sync"
	"testing"
)

func TestQueueFIFOAndStop(t *testing.T) {
	q := New[int]()
	nodes := []*Node[int]{{Value: 1}, {Value: 2}, {Value: 3}}
	if !q.Push(nodes[0]) || !q.Push(nodes[1]) || !q.Stop(nodes[2]) {
		t.Fatal("queue rejected work before stop")
	}
	if q.Push(&Node[int]{Value: 4}) {
		t.Fatal("queue accepted work after stop")
	}

	var got []int
	for node := q.Drain(); node != nil; node = node.TakeNext() {
		got = append(got, node.Value)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("drain order = %v", got)
	}
}

func TestQueueConcurrentProducers(t *testing.T) {
	q := New[int]()
	const producers = 8
	const perProducer = 100
	var wg sync.WaitGroup
	for producer := 0; producer < producers; producer++ {
		wg.Add(1)
		go func(producer int) {
			defer wg.Done()
			for sequence := 0; sequence < perProducer; sequence++ {
				if !q.Push(&Node[int]{Value: producer*perProducer + sequence}) {
					t.Errorf("Push rejected producer %d sequence %d", producer, sequence)
					return
				}
			}
		}(producer)
	}
	wg.Wait()

	seen := make(map[int]bool, producers*perProducer)
	lastSequence := make([]int, producers)
	for producer := range lastSequence {
		lastSequence[producer] = -1
	}
	for node := q.Drain(); node != nil; node = node.TakeNext() {
		value := node.Value
		if seen[value] {
			t.Fatalf("duplicate value %d", value)
		}
		seen[value] = true
		producer, sequence := value/perProducer, value%perProducer
		if sequence != lastSequence[producer]+1 {
			t.Fatalf("producer %d order jumped from %d to %d", producer, lastSequence[producer], sequence)
		}
		lastSequence[producer] = sequence
	}
	if len(seen) != producers*perProducer {
		t.Fatalf("drained %d nodes, want %d", len(seen), producers*perProducer)
	}
}

func TestQueueStateTransitions(t *testing.T) {
	q := New[string]()
	if !q.Accepting() {
		t.Fatal("new queue is not accepting")
	}
	if q.HasPending() {
		t.Fatal("new queue has pending work")
	}
	if !q.Push(&Node[string]{Value: "work"}) || !q.HasPending() {
		t.Fatal("pushed work is not pending")
	}
	if q.Drain() == nil || q.HasPending() {
		t.Fatal("Drain did not empty queue")
	}
	if !q.Stop(&Node[string]{Value: "stop"}) {
		t.Fatal("Stop rejected final node")
	}
	if q.Accepting() {
		t.Fatal("stopped queue is still accepting")
	}
	if q.Stop(&Node[string]{Value: "duplicate"}) {
		t.Fatal("second Stop succeeded")
	}
}

func TestQueueRejectsInvalidNodes(t *testing.T) {
	tests := []struct {
		name string
		node *Node[int]
	}{
		{name: "nil"},
		{name: "linked", node: &Node[int]{next: &Node[int]{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Push did not panic")
				}
			}()
			New[int]().Push(test.node)
		})
	}
}
