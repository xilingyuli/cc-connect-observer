package observer

import (
	"strings"
	"testing"
	"time"
)

func msg(session, sender, content string, mentioned bool) IncomingMessage {
	return IncomingMessage{SessionKey: session, UserName: sender, Content: content, Mentioned: mentioned}
}

func newTestObserver(cfg Config) *Observer {
	cfg.WindowSec = 0 // tests opt into the engage window explicitly
	return New(cfg)
}

func TestAtMessageAlwaysForwardsWithHighEffort(t *testing.T) {
	o := newTestObserver(defaultConfig())
	d := o.Decide(msg("qq:g:1", "A", "推荐几个好玩的游戏", true))
	if !d.Forward || d.Content != "推荐几个好玩的游戏" || d.Effort != "high" || d.SuppressProgress {
		t.Fatalf("at decision wrong: %+v", d)
	}
}

func TestKeywordActivationForwardsShadow(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	d := o.Decide(msg("qq:g:1", "A", "你觉得什么游戏好玩", false))
	if !d.Forward {
		t.Fatal("keyword message should forward as shadow turn")
	}
	if d.Effort != "low" || !d.SuppressProgress {
		t.Fatalf("shadow decision should use low effort + suppress progress: %+v", d)
	}
	for _, want := range []string{"影子人格", "本次触发", "NO_REPLY", "最值得回复", "不透露存在"} {
		if !strings.Contains(d.Content, want) {
			t.Fatalf("shadow prompt missing %q", want)
		}
	}
}

func TestNoKeywordNoForward(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	if d := o.Decide(msg("qq:g:1", "A", "今天天气不错", false)); d.Forward {
		t.Fatal("non-keyword message should not forward outside engage window")
	}
}

func TestIgnorePatternsExcludeMessages(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)
	o.SetIgnorePatterns([]string{`^#`, `^\*`})

	if d := o.Decide(msg("qq:g:1", "A", "#为什么更新不了", false)); d.Forward {
		t.Fatal("ignored message should not forward")
	}
	if d := o.Decide(msg("qq:g:1", "A", "为什么更新不了", false)); !d.Forward {
		t.Fatal("non-ignored keyword message should forward")
	}
}

func TestMultiUserRuleActivates(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.Rules = []Rule{{Type: "multi_user", MinUsers: 4, WindowSec: 300}}
	o := newTestObserver(cfg)

	// Three distinct participants: not enough.
	o.Decide(msg("qq:g:1", "A", "今天天气不错", false))
	o.Decide(msg("qq:g:1", "B", "是啊挺热的", false))
	o.Decide(msg("qq:g:1", "C", "晚上还凉快", false))
	if d := o.Decide(msg("qq:g:1", "C", "还是C", false)); d.Forward {
		t.Fatal("3 distinct participants should not activate multi_user(min=4)")
	}

	// A fourth distinct participant crosses the threshold.
	if d := o.Decide(msg("qq:g:1", "D", "明天降温", false)); !d.Forward {
		t.Fatal("4 distinct participants should activate multi_user(min=4)")
	}
}

func TestEngageWindowForwardsWithoutKeyword(t *testing.T) {
	cfg := defaultConfig()
	cfg.WindowSec = 600
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := New(cfg)
	o.OnGroupReply("qq:g:1")

	if d := o.Decide(msg("qq:g:1", "A", "今天天气不错", false)); !d.Forward {
		t.Fatal("engage window should forward without keyword")
	}
}

func TestEngageWindowExpiresWithoutKeyword(t *testing.T) {
	cfg := defaultConfig()
	cfg.WindowSec = 600
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := New(cfg)

	o.mu.Lock()
	st := &groupState{}
	st.replies = []replyEvent{{At: time.Now().Add(-20 * time.Minute), Kind: "proactive"}}
	o.groups["qq:g:1"] = st
	o.mu.Unlock()

	if d := o.Decide(msg("qq:g:1", "A", "今天天气不错", false)); d.Forward {
		t.Fatal("outside engage window without keyword should not forward")
	}
}

func TestCatchUpSupplementsMissedMessages(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	// M1 @ -> forwarded, sets the forwarding point.
	d1 := o.Decide(msg("qq:g:1", "A", "M1at消息", true))
	if !d1.Forward {
		t.Fatal("M1 should forward")
	}
	// M2/M3 missed (no keyword).
	if o.Decide(msg("qq:g:1", "B", "M2闲聊内容", false)).Forward {
		t.Fatal("M2 should not forward")
	}
	if o.Decide(msg("qq:g:1", "C", "M3闲聊内容", false)).Forward {
		t.Fatal("M3 should not forward")
	}
	// M4 triggers shadow: prompt must include M2/M3, not M1.
	d4 := o.Decide(msg("qq:g:1", "D", "你们怎么看待这个", false))
	if !d4.Forward {
		t.Fatal("M4 should forward")
	}
	for _, want := range []string{"M2闲聊内容", "M3闲聊内容", "你们怎么看待这个"} {
		if !strings.Contains(d4.Content, want) {
			t.Fatalf("shadow prompt missing %q", want)
		}
	}
	if strings.Contains(d4.Content, "M1at消息") {
		t.Fatal("already-forwarded M1 should not be re-supplemented")
	}
}

func TestCatchUpWindowExcludesOldMessages(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.WindowSec = 600
	o := New(cfg)
	now := time.Now()
	st := &groupState{lastForwardedAt: now.Add(-3 * time.Hour)}
	st.buf = append(st.buf, ObservedMessage{At: now.Add(-2 * time.Hour), Sender: "X", Text: "很旧的消息"})
	o.mu.Lock()
	o.groups["qq:g:1"] = st
	o.mu.Unlock()

	d := o.Decide(msg("qq:g:1", "D", "你们怎么看", false))
	if d.Forward && strings.Contains(d.Content, "很旧的消息") {
		t.Fatal("old message outside catchup window should be excluded")
	}
}

func TestOnGroupReplyRecordsProactive(t *testing.T) {
	cfg := defaultConfig()
	cfg.WindowSec = 600
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := New(cfg)

	// A reply before any observation is ignored; after one Decide the group exists.
	o.Decide(msg("qq:g:1", "A", "为什么", false))
	o.OnGroupReply("qq:g:1")

	o.mu.Lock()
	st := o.groups["qq:g:1"]
	o.mu.Unlock()
	if st == nil || lastProactive(st).IsZero() {
		t.Fatal("OnGroupReply should record a proactive reply")
	}
}

func TestMentionedRecordsAtReply(t *testing.T) {
	o := newTestObserver(defaultConfig())
	o.Decide(msg("qq:g:1", "A", "在吗", true))
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	o.mu.Unlock()
	if st == nil || len(st.replies) != 1 || st.replies[0].Kind != "at" {
		t.Fatalf("at event not recorded: %+v", st)
	}
}

func TestNonAtPendingWhenBusy(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := New(cfg)
	o.SetSessionBusy(func(string) bool { return true })

	d := o.Decide(msg("qq:g:1", "A", "你们觉得什么好玩", false))
	if !d.Pending || d.Forward {
		t.Fatalf("busy non-@ message should pend: %+v", d)
	}
}

func TestAtNotPendingWhenBusy(t *testing.T) {
	cfg := defaultConfig()
	o := New(cfg)
	o.SetSessionBusy(func(string) bool { return true })

	d := o.Decide(msg("qq:g:1", "A", "推荐个游戏", true))
	if d.Pending || !d.Forward {
		t.Fatalf("@ message should decide immediately even when busy: %+v", d)
	}
}

func TestPendingDecidedAfterReply(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := New(cfg)
	busy := true
	o.SetSessionBusy(func(string) bool { return busy })
	var forwarded []Decision
	o.SetForwarder(func(_ string, d Decision) error {
		forwarded = append(forwarded, d)
		return nil
	})

	if d := o.Decide(msg("qq:g:1", "A", "你们觉得什么好玩", false)); !d.Pending {
		t.Fatalf("should pend while busy: %+v", d)
	}

	// The in-flight turn completes with a real reply -> pending re-decided.
	busy = false
	o.OnGroupReply("qq:g:1")
	if len(forwarded) != 1 || !forwarded[0].Forward || forwarded[0].Effort != "low" {
		t.Fatalf("pending should be forwarded after reply: %v", forwarded)
	}
}

func TestPendingStaleDropped(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.WindowSec = 600
	o := New(cfg)
	busy := true
	o.SetSessionBusy(func(string) bool { return busy })
	var forwarded []Decision
	o.SetForwarder(func(_ string, d Decision) error {
		forwarded = append(forwarded, d)
		return nil
	})

	if d := o.Decide(msg("qq:g:1", "A", "你们觉得什么好玩", false)); !d.Pending {
		t.Fatal("should pend while busy")
	}
	// Age the pending message beyond the catchup window.
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	st.pending[0].entry.At = time.Now().Add(-2 * time.Hour)
	o.mu.Unlock()

	busy = false
	o.OnGroupReply("qq:g:1")
	if len(forwarded) != 0 {
		t.Fatalf("stale pending message should be dropped, got %d forwards", len(forwarded))
	}
}

func TestPendingOnlyFirstForwardedRestWaits(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := New(cfg)
	busy := true
	o.SetSessionBusy(func(string) bool { return busy })
	var forwarded []Decision
	o.SetForwarder(func(_ string, d Decision) error {
		forwarded = append(forwarded, d)
		return nil
	})

	o.Decide(msg("qq:g:1", "A", "你们觉得什么好玩", false))
	o.Decide(msg("qq:g:1", "B", "这是第二个话题，你们怎么看", false))

	busy = false
	o.OnGroupReply("qq:g:1")
	if len(forwarded) != 1 {
		t.Fatalf("only the first pending should forward, got %d", len(forwarded))
	}
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	remaining := len(st.pending)
	o.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("second pending should stay queued, remaining=%d", remaining)
	}
}

func TestCatchUpCharCap(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.CatchupMaxChars = 10
	o := newTestObserver(cfg)

	o.Decide(msg("qq:g:1", "A", "M1at消息", true))
	o.Decide(msg("qq:g:1", "B", "很长很长的闲聊内容AAAAAAAAAA", false))
	d := o.Decide(msg("qq:g:1", "C", "你们怎么看", false))
	if !d.Forward {
		t.Fatal("should forward")
	}
	if strings.Contains(d.Content, "很长很长的闲聊内容") {
		t.Fatal("catchup text beyond char cap should be dropped")
	}
}
