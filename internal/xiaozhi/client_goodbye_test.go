package xiaozhi

import "testing"

func TestDrainEventChannelsKeepsGoodbye(t *testing.T) {
	c := &Client{
		sttCh:     make(chan string, 1),
		ttsCh:     make(chan ServerMessage, 1),
		llmCh:     make(chan ServerMessage, 1),
		audioCh:   make(chan []byte, 1),
		goodbyeCh: make(chan struct{}, 1),
	}
	c.goodbyeCh <- struct{}{}
	c.sttCh <- "stale"
	c.DrainEventChannels()
	if !c.TakeGoodbye() {
		t.Fatal("DrainEventChannels consumed goodbye")
	}
	select {
	case <-c.sttCh:
		t.Fatal("stale STT should have been drained")
	default:
	}
}

func TestTakeGoodbyeEmpty(t *testing.T) {
	c := &Client{goodbyeCh: make(chan struct{}, 1)}
	if c.TakeGoodbye() {
		t.Fatal("empty goodbyeCh should be false")
	}
}
