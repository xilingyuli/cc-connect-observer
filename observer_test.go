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

func TestAtMessageForwardsDirectWithMediumEffort(t *testing.T) {
	o := newTestObserver(defaultConfig())
	d := o.Decide(msg("qq:g:1", "A", "推荐几个好玩的游戏", true))
	if !d.Forward || d.Effort != "medium" || !d.SuppressProgress {
		t.Fatalf("at decision wrong: %+v", d)
	}
	for _, want := range []string{"你的身份", "被 @ 的直接对话", "积极回应"} {
		if !strings.Contains(d.Content, want) {
			t.Fatalf("direct prompt missing %q", want)
		}
	}
	if strings.Contains(d.Content, "最值得回复的前") {
		t.Fatal("direct turn should not carry the top-X% judgment")
	}
}

func TestKeywordActivationForwardsProactive(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	d := o.Decide(msg("qq:g:1", "A", "你觉得什么游戏好玩", false))
	if !d.Forward {
		t.Fatal("keyword message should forward as proactive turn")
	}
	if d.Effort != "low" || !d.SuppressProgress {
		t.Fatalf("proactive decision should use low effort + suppress progress: %+v", d)
	}
	for _, want := range []string{"你的身份", "排队", "NO_REPLY", "选择性回复"} {
		if !strings.Contains(d.Content, want) {
			t.Fatalf("proactive prompt missing %q", want)
		}
	}
	for _, forbidden := range []string{"最多 5 条", "口语化", "简短"} {
		if strings.Contains(d.Content, forbidden) {
			t.Fatalf("proactive prompt should not contain %q", forbidden)
		}
	}
	if !d.SplitReplies {
		t.Fatal("proactive batch decision should carry SplitReplies")
	}
}

func TestAliasMentionForwardsDirect(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.PersonaAlias = "小鲸鱼"
	o := newTestObserver(cfg)

	d := o.Decide(msg("qq:g:1", "A", "小鲸鱼在吗", false))
	if !d.Forward || d.Effort != "medium" || !d.SuppressProgress {
		t.Fatalf("alias mention should forward as direct conversation: %+v", d)
	}
	for _, want := range []string{"先结合上下文判断", "积极回应", "常叫你“小鲸鱼”", "只是提到/转述"} {
		if !strings.Contains(d.Content, want) {
			t.Fatalf("direct prompt missing %q", want)
		}
	}
}

func TestPersonaAliasInPrompt(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.PersonaAlias = "小鲸鱼"
	o := newTestObserver(cfg)

	d := o.Decide(msg("qq:g:1", "A", "你们觉得什么好玩", false))
	if !d.Forward || !strings.Contains(d.Content, "常叫你“小鲸鱼”") {
		t.Fatalf("alias should be mentioned in the persona prompt: forward=%v", d.Forward)
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

func TestEngageWindowAfterAtReplyForwardsFollowUp(t *testing.T) {
	cfg := defaultConfig()
	cfg.WindowSec = 600
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := New(cfg)

	// @ direct turn, then the bot actually replies to it.
	if d := o.Decide(msg("qq:g:1", "A", "在吗", true)); !d.Forward {
		t.Fatal("@ message should forward")
	}
	o.OnGroupReply("qq:g:1")

	// Follow-up without @ / alias / keyword must still forward as a
	// continuation within the engage window opened by the @ reply.
	d := o.Decide(msg("qq:g:1", "A", "继续展开说说", false))
	if !d.Forward {
		t.Fatal("follow-up after @ reply should forward within engage window")
	}
	for _, want := range []string{"时间", "与自己对", "积极回应"} {
		if !strings.Contains(d.Content, want) {
			t.Fatalf("engage prompt missing %q", want)
		}
	}
	if strings.Contains(d.Content, "被 @ 的直接对话") {
		t.Fatal("engage follow-up should not be framed as an @ direct turn")
	}
}

func TestEngageWindowAfterAliasReplyForwardsFollowUp(t *testing.T) {
	cfg := defaultConfig()
	cfg.WindowSec = 600
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.PersonaAlias = "小鲸鱼"
	o := New(cfg)

	if d := o.Decide(msg("qq:g:1", "A", "小鲸鱼在吗", false)); !d.Forward {
		t.Fatal("alias mention should forward")
	}
	o.OnGroupReply("qq:g:1")

	d := o.Decide(msg("qq:g:1", "A", "然后呢", false))
	if !d.Forward {
		t.Fatal("follow-up after alias reply should forward within engage window")
	}
	if !strings.Contains(d.Content, "与自己对") {
		t.Fatalf("engage prompt missing continuation framing: %+v", d)
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

func TestBatchIncludesEarlierPendingMessages(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	// M1 @ -> forwarded immediately.
	d1 := o.Decide(msg("qq:g:1", "A", "M1at消息", true))
	if !d1.Forward {
		t.Fatal("M1 should forward")
	}
	// M2/M3 have no keyword: they stay queued, no turn yet.
	if o.Decide(msg("qq:g:1", "B", "M2闲聊内容", false)).Forward {
		t.Fatal("M2 should not forward")
	}
	if o.Decide(msg("qq:g:1", "C", "M3闲聊内容", false)).Forward {
		t.Fatal("M3 should not forward")
	}
	// M4 triggers a batch turn: the whole queue is merged, M1 not included.
	d4 := o.Decide(msg("qq:g:1", "D", "你们怎么看待这个", false))
	if !d4.Forward {
		t.Fatal("M4 should forward")
	}
	for _, want := range []string{"M2闲聊内容", "M3闲聊内容", "你们怎么看待这个"} {
		if !strings.Contains(d4.Content, want) {
			t.Fatalf("proactive prompt missing %q", want)
		}
	}
	if strings.Contains(d4.Content, "M1at消息") {
		t.Fatal("already-forwarded M1 should not be re-included")
	}
}

func TestBatchExcludesStalePending(t *testing.T) {
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

	o.Decide(msg("qq:g:1", "A", "很旧的话题你们怎么看", false))
	o.Decide(msg("qq:g:1", "B", "新的话题你们怎么看", false))

	// Age only the first pending entry (and its buffer twin) beyond the window.
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	st.pending[0].entry.At = time.Now().Add(-2 * time.Hour)
	for i := range st.buf {
		if st.buf[i].Text == "很旧的话题你们怎么看" {
			st.buf[i].At = time.Now().Add(-2 * time.Hour)
		}
	}
	o.mu.Unlock()

	busy = false
	o.OnGroupReply("qq:g:1")
	if len(forwarded) != 1 {
		t.Fatalf("expected one batch decision, got %d", len(forwarded))
	}
	if strings.Contains(forwarded[0].Content, "很旧的话题") {
		t.Fatal("stale pending message should be excluded from the batch")
	}
	if !strings.Contains(forwarded[0].Content, "新的话题") {
		t.Fatal("fresh pending message should be included in the batch")
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

func TestPendingDecidedAfterSilentTurn(t *testing.T) {
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

	// The in-flight turn completes with NO_REPLY: the pending queue must still
	// be re-evaluated instead of waiting for the next message.
	busy = false
	o.OnSilentTurn("qq:g:1")
	if len(forwarded) != 1 || !forwarded[0].Forward {
		t.Fatalf("pending should be forwarded after silent turn: %v", forwarded)
	}
}

func TestOnSilentTurnDoesNotRecordReply(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	o.Decide(msg("qq:g:1", "A", "为什么", false)) // ensure group state exists
	o.OnSilentTurn("qq:g:1")

	o.mu.Lock()
	st := o.groups["qq:g:1"]
	o.mu.Unlock()
	if st == nil || len(st.replies) != 0 {
		t.Fatalf("silent turn must not record a reply event: %+v", st)
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

func TestPendingMergedIntoSingleBatch(t *testing.T) {
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
		t.Fatalf("pending should merge into one batch decision, got %d", len(forwarded))
	}
	if !forwarded[0].SplitReplies {
		t.Fatal("batch decision should carry SplitReplies")
	}
	for _, want := range []string{"你们觉得什么好玩", "这是第二个话题"} {
		if !strings.Contains(forwarded[0].Content, want) {
			t.Fatalf("batch prompt missing %q", want)
		}
	}
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	remaining := len(st.pending)
	o.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("batch should consume the whole queue, remaining=%d", remaining)
	}
}

func TestNewMessageJoinsPendingBatch(t *testing.T) {
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

	// First candidate pends while busy.
	if d := o.Decide(msg("qq:g:1", "A", "你们觉得什么好玩", false)); !d.Pending {
		t.Fatalf("first message should pend while busy: %+v", d)
	}
	// Session frees; a new message arrives: the whole queue merges into one
	// batch decision returned directly, nothing stays queued.
	busy = false
	d := o.Decide(msg("qq:g:1", "B", "还有什么推荐", false))
	if !d.Forward {
		t.Fatalf("free-path batch should forward: %+v", d)
	}
	if len(forwarded) != 0 {
		t.Fatalf("free-path decision should be returned directly, not via forwarder: %d", len(forwarded))
	}
	if !d.SplitReplies {
		t.Fatal("free-path batch decision should carry SplitReplies")
	}
	for _, want := range []string{"你们觉得什么好玩", "还有什么推荐"} {
		if !strings.Contains(d.Content, want) {
			t.Fatalf("batch decision should merge old and new pending, missing %q", want)
		}
	}
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	remaining := len(st.pending)
	o.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("batch should consume the whole queue, remaining=%d", remaining)
	}
}

func TestBatchMaxMsgsCap(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.MaxBatchMsgs = 2
	o := New(cfg)
	busy := true
	o.SetSessionBusy(func(string) bool { return busy })
	var forwarded []Decision
	o.SetForwarder(func(_ string, d Decision) error {
		forwarded = append(forwarded, d)
		return nil
	})

	o.Decide(msg("qq:g:1", "A", "旧一你们怎么看", false))
	o.Decide(msg("qq:g:1", "B", "旧二你们怎么看", false))
	o.Decide(msg("qq:g:1", "C", "最新你们怎么看", false))

	busy = false
	o.OnGroupReply("qq:g:1")
	if len(forwarded) != 1 {
		t.Fatalf("expected one batch decision, got %d", len(forwarded))
	}
	// The over-cap message must not be a candidate, though it may appear in the
	// background context section.
	candStart := strings.Index(forwarded[0].Content, "【本次待处理的排队消息】")
	candEnd := strings.Index(forwarded[0].Content, "【补充上下文")
	if candStart < 0 || candEnd < candStart {
		t.Fatalf("unexpected batch prompt layout: %s", previewContent(forwarded[0].Content))
	}
	candSection := forwarded[0].Content[candStart:candEnd]
	if strings.Contains(candSection, "旧一") {
		t.Fatal("messages beyond max_batch_msgs should be dropped from the candidates")
	}
	for _, want := range []string{"旧二", "最新"} {
		if !strings.Contains(candSection, want) {
			t.Fatalf("batch candidates missing %q", want)
		}
	}
}

func TestNonActivatingDroppedFromPendingKeptAsContext(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	// Non-keyword message: no turn, dropped from pending (buffer keeps it).
	if d := o.Decide(msg("qq:g:1", "A", "今天天气不错", false)); d.Forward {
		t.Fatal("non-keyword message should not forward")
	}
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	pending := len(st.pending)
	bufLen := len(st.buf)
	o.mu.Unlock()
	if pending != 0 {
		t.Fatalf("non-activating message should be dropped from pending, pending=%d", pending)
	}
	if bufLen == 0 {
		t.Fatal("non-activating message should stay in the rolling buffer as context")
	}

	// A later keyword batch should see it in the background context.
	d := o.Decide(msg("qq:g:1", "B", "你们怎么看这个", false))
	if !d.Forward {
		t.Fatal("keyword message should forward as a batch")
	}
	if !strings.Contains(d.Content, "今天天气不错") || !strings.Contains(d.Content, "补充上下文") {
		t.Fatalf("earlier message should appear as batch context: %+v", d.Content)
	}
}

func TestBatchPromptCarriesSenderQQAndNoNamePrefix(t *testing.T) {
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

	o.Decide(IncomingMessage{SessionKey: "qq:g:1", UserName: "小明", UserID: "123456", Content: "你们怎么看", Mentioned: false})
	busy = false
	o.OnGroupReply("qq:g:1")
	if len(forwarded) != 1 {
		t.Fatalf("expected one batch decision, got %d", len(forwarded))
	}
	for _, want := range []string{"QQ 123456", "小明", "get_msg"} {
		if !strings.Contains(forwarded[0].Content, want) {
			t.Fatalf("batch prompt missing %q", want)
		}
	}
	for _, forbidden := range []string{"开头点出对方名字", "不要用其他分隔符", "不要列条陈词", "[CQ:at", "最多", "口语化", "简短", "config.toml", ".codex"} {
		if strings.Contains(forwarded[0].Content, forbidden) {
			t.Fatalf("batch prompt should not contain %q", forbidden)
		}
	}
}

func TestRealMessageTimeUsedForEntry(t *testing.T) {
	o := newTestObserver(defaultConfig())
	msgAt := time.Now().Add(-3 * time.Minute)
	o.Decide(IncomingMessage{
		SessionKey:        "qq:g:1",
		UserName:          "A",
		Content:           "你们怎么看",
		Mentioned:         false,
		UserMessageTimeMs: msgAt.UnixMilli(),
	})

	o.mu.Lock()
	st := o.groups["qq:g:1"]
	o.mu.Unlock()
	if len(st.buf) == 0 {
		t.Fatal("message should be observed")
	}
	if !st.buf[0].At.Equal(time.UnixMilli(msgAt.UnixMilli())) {
		t.Fatalf("entry time = %v, want %v (real message time)", st.buf[0].At, msgAt)
	}
}

func TestMediaOnlyMessageVisibleAsContextNotJudged(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	// Media-only message: observed as context, no turn triggered by itself.
	if d := o.Decide(IncomingMessage{SessionKey: "qq:g:1", UserName: "A", MessageID: "10086", MediaKind: "图片"}); d.Forward {
		t.Fatal("media-only message should not trigger a turn by itself")
	}
	// A later keyword batch includes it as context with its id and kind.
	d := o.Decide(msg("qq:g:1", "B", "你们怎么看", false))
	if !d.Forward {
		t.Fatal("keyword message should forward")
	}
	for _, want := range []string{"图片 id 10086", "媒体消息", "不需要判断是否回复"} {
		if !strings.Contains(d.Content, want) {
			t.Fatalf("batch prompt missing %q", want)
		}
	}
}

func TestQuotedMessageIDInPrompt(t *testing.T) {
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

	o.Decide(IncomingMessage{SessionKey: "qq:g:1", UserName: "小明", UserID: "123", Content: "这条是回复，你们怎么看", QuotedMessageID: "888"})
	busy = false
	o.OnGroupReply("qq:g:1")
	if len(forwarded) != 1 {
		t.Fatalf("expected one batch decision, got %d", len(forwarded))
	}
	if !strings.Contains(forwarded[0].Content, "引用 id 888") {
		t.Fatalf("batch prompt missing quoted message id: %s", previewContent(forwarded[0].Content))
	}
}

func previewContent(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
