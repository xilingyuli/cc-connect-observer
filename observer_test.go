package observer

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type jobRecorder struct {
	mu   sync.Mutex
	jobs []*core.CronJob
}

func (r *jobRecorder) add(j *core.CronJob) {
	r.mu.Lock()
	r.jobs = append(r.jobs, j)
	r.mu.Unlock()
}

func (r *jobRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.jobs)
}

func (r *jobRecorder) last() *core.CronJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.jobs) == 0 {
		return nil
	}
	return r.jobs[len(r.jobs)-1]
}

func newTestObserver(cfg Config) (*Observer, *jobRecorder) {
	o := New(nil, cfg)
	rec := &jobRecorder{}
	o.SetRunner(func(j *core.CronJob) error {
		rec.add(j)
		return nil
	})
	return o, rec
}

func TestKeywordActivation(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o, rec := newTestObserver(cfg)

	o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "A", Content: "今天为什么不下雨"})
	o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "B", Content: "今天天气不错"})
	time.Sleep(80 * time.Millisecond)

	if n := rec.count(); n != 1 {
		t.Fatalf("keyword activation: got %d observation turns, want 1", n)
	}
	job := rec.last()
	if !strings.Contains(job.Prompt, "NO_REPLY") || !strings.Contains(job.Prompt, "群聊记录") {
		t.Fatalf("observation prompt missing liveliness/NO_REPLY instructions: %q", job.Prompt)
	}
	if job.SessionKey != "qq:g:1" || job.SessionMode != "new_per_run" || !job.SuppressProgress || job.Silent == nil || !*job.Silent {
		t.Fatalf("observation job fields wrong: %+v", job)
	}
}

func TestIgnorePatternsExcludeMessages(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o, rec := newTestObserver(cfg)
	o.SetIgnorePatterns([]string{`^#`, `^\*`})

	o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "A", Content: "#为什么更新不了"})
	o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "A", Content: "为什么更新不了"})
	time.Sleep(80 * time.Millisecond)

	if n := rec.count(); n != 1 {
		t.Fatalf("ignore patterns: got %d turns, want 1 (only non-ignored activates)", n)
	}
}

func TestCooldownBlocksRepeatedActivation(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 3600
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o, rec := newTestObserver(cfg)

	o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "A", Content: "为什么不下雨"})
	o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "B", Content: "这个为什么这样"})
	time.Sleep(80 * time.Millisecond)

	if n := rec.count(); n != 1 {
		t.Fatalf("cooldown: got %d turns, want 1", n)
	}
}

func TestMentionRecordedAndAnnotated(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o, rec := newTestObserver(cfg)

	o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "A", Content: "在吗", Mentioned: true})
	o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "B", Content: "你们觉得呢"})
	time.Sleep(80 * time.Millisecond)

	if n := rec.count(); n != 1 {
		t.Fatalf("expected 1 observation turn, got %d", n)
	}
	if p := rec.last().Prompt; !strings.Contains(p, "@回复") {
		t.Fatalf("prompt should annotate recent @ reply times: %q", p)
	}
}

func TestBufferCapAndContext(t *testing.T) {
	cfg := defaultConfig()
	cfg.BufferSize = 2
	o, _ := newTestObserver(cfg)

	for i := 0; i < 3; i++ {
		o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "A", Content: "消息"})
	}
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	o.mu.Unlock()
	if st == nil || len(st.buf) != 2 {
		t.Fatalf("buffer cap: got %d buffered, want 2", len(st.buf))
	}
	if got := o.contextMsgs(st); len(got) != 2 {
		t.Fatalf("context msgs: got %d, want 2", len(got))
	}
}

func TestMaxPerHourBlocks(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 1
	o, _ := newTestObserver(cfg)

	st := &groupState{}
	now := time.Now()
	st.replies = []replyEvent{
		{At: now.Add(-10 * time.Minute), Kind: "proactive"},
	}
	o.mu.Lock()
	o.groups["qq:g:1"] = st
	o.mu.Unlock()

	o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "A", Content: "为什么"})
	time.Sleep(80 * time.Millisecond)
	if !st.lastAct.IsZero() {
		t.Fatal("max_per_hour should block activation when hourly limit reached")
	}
}

func TestProactiveEventRecordedAfterRun(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o, _ := newTestObserver(cfg)

	o.OnGroupMessage(&core.Message{SessionKey: "qq:g:1", UserName: "A", Content: "为什么"})
	time.Sleep(80 * time.Millisecond)
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	o.mu.Unlock()
	if st == nil || lastProactive(st).IsZero() {
		t.Fatal("proactive reply event should be recorded after an observation run")
	}
}
