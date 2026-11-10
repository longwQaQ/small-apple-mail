package events

import (
	"testing"
	"time"
)

func TestHubHistoryReconnectAndSlowSubscriber(t *testing.T) {
	hub := New()
	first := hub.Publish("job.created", map[string]string{"id": "one"})
	second := hub.Publish("job.progress", map[string]string{"id": "one"})
	ch, unsubscribe := hub.Subscribe(first.ID)
	defer unsubscribe()
	select {
	case event := <-ch:
		if event.ID != second.ID || event.Type != "job.progress" {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("没有收到重连历史")
	}

	slow, closeSlow := hub.Subscribe(0)
	defer closeSlow()
	started := time.Now()
	for i := 0; i < 700; i++ {
		hub.Publish("burst", i)
	}
	if time.Since(started) > time.Second {
		t.Fatal("慢订阅者阻塞了发布")
	}
	_ = slow
	if len(hub.history) != 500 {
		t.Fatalf("history=%d", len(hub.history))
	}
	reconnected, closeReconnected := hub.Subscribe(second.ID)
	defer closeReconnected()
	for i := 0; i < 500; i++ {
		select {
		case <-reconnected:
		case <-time.After(time.Second):
			t.Fatalf("重连历史在第 %d 条阻塞", i)
		}
	}
}

func TestNewSubscriberOnlyReceivesLiveEvents(t *testing.T) {
	hub := New()
	for i := 0; i < 500; i++ {
		hub.Publish("job.completed", i)
	}
	ch, unsubscribe := hub.Subscribe(0)
	defer unsubscribe()
	if len(ch) != 0 {
		t.Fatalf("首次订阅不应重放历史事件，收到 %d 条", len(ch))
	}
	want := hub.Publish("mailbox.changed", nil)
	select {
	case got := <-ch:
		if got.ID != want.ID {
			t.Fatalf("实时事件 ID=%d，期望 %d", got.ID, want.ID)
		}
	default:
		t.Fatal("未收到实时事件")
	}
}

func BenchmarkHubPublishWithFullHistory(b *testing.B) {
	hub := New()
	for i := 0; i < 500; i++ {
		hub.Publish("job.completed", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		hub.Publish("job.completed", 1)
	}
}
