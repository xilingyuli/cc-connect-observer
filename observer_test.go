package observer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func msg(session, sender, content string, mentioned bool) IncomingMessage {
	return IncomingMessage{SessionKey: session, UserName: sender, Content: content, Mentioned: mentioned}
}

func msgAt(session, sender, content string, mentioned bool, localISO string) IncomingMessage {
	t := mustLocal(localISO)
	m := msg(session, sender, content, mentioned)
	m.UserMessageTimeMs = t.UnixMilli()
	return m
}

func mustLocal(localISO string) time.Time {
	t, err := time.ParseInLocation("2006-01-02T15:04:05", localISO, time.FixedZone("CST", 8*3600))
	if err != nil {
		panic(err)
	}
	return t
}

func newTestObserver(cfg Config) *Observer {
	cfg.WindowSec = 0 // tests opt into the engage window explicitly
	cfg = withTestPrompts(cfg)
	return New(cfg)
}

func newTestObserverWithWindow(cfg Config) *Observer {
	cfg = withTestPrompts(cfg)
	return New(cfg)
}

func withTestPrompts(cfg Config) Config {
	prompts, err := loadGroupPrompts("group_chat.json")
	if err != nil {
		panic(err)
	}
	cfg.GroupPrompts = prompts
	return cfg
}

func TestLoadConfigLoadsGroupPromptMapRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	promptData, err := os.ReadFile("group_chat.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "group_chat.json"), promptData, 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "observer.toml")
	if err := os.WriteFile(configPath, []byte(`group_prompt_file = "group_chat.json"`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.GroupPrompts[promptBlockRequirementsAt]; !strings.Contains(got, "被 @ 的直接对话") {
		t.Fatalf("prompt map not loaded: %q", got)
	}
}

func TestLoadGroupPromptsRejectsMissingRequiredKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "group_chat.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGroupPrompts(path); err == nil || !strings.Contains(err.Error(), "missing required key") {
		t.Fatalf("expected missing key error, got %v", err)
	}
}

func TestPromptPlaceholderReplacementIsWeak(t *testing.T) {
	cfg := defaultConfig()
	o := newTestObserver(cfg)

	got := renderPromptTemplate("{{persona_name}} {{persona_alias}} {{unknown}}", map[string]string{
		"persona_name":  "小鲸鱼",
		"persona_alias": "",
		"unknown":       "不应替换",
	})
	if got != "小鲸鱼 {{persona_alias}} {{unknown}}" {
		t.Fatalf("weak replacement result = %q", got)
	}
	if got := o.prompt(promptBlockIdentity); !strings.Contains(got, "{{persona_name}}") || !strings.Contains(got, "{{persona_alias}}") {
		t.Fatalf("empty configured names should remain as placeholders: %q", got)
	}
}

func TestAtMessageForwardsDirectWithMediumEffort(t *testing.T) {
	o := newTestObserver(defaultConfig())
	d := o.Decide(msg("qq:g:1", "A", "推荐几个好玩的游戏", true))
	if !d.Forward || d.Effort != "medium" || !d.SuppressProgress || !d.SplitReplies || !d.Direct {
		t.Fatalf("at decision wrong: %+v", d)
	}
	for _, want := range []string{"你的身份", "被 @ 的直接对话", "积极回应"} {
		if !strings.Contains(d.Content, want) {
			t.Fatalf("direct prompt missing %q", want)
		}
	}
	if !strings.Contains(d.Content, "思考过程不要写进回复") {
		t.Fatal("direct prompt should carry the common thinking-guard rule")
	}
	if strings.Contains(d.Content, "最值得回复的前") {
		t.Fatal("高优回合不应携带前 X% 参与度判断")
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
	for _, want := range []string{"你的身份", "排队", "NO_REPLY", "选择性回复", "最值得回复的前 20%"} {
		if !strings.Contains(d.Content, want) {
			t.Fatalf("proactive prompt missing %q", want)
		}
	}
	for _, forbidden := range []string{"最多 5 条"} {
		if strings.Contains(d.Content, forbidden) {
			t.Fatalf("proactive prompt should not contain %q", forbidden)
		}
	}
	if !d.SplitReplies {
		t.Fatal("proactive batch decision should carry SplitReplies")
	}
	if d.Direct {
		t.Fatal("proactive batch decision should not be marked Direct")
	}
}

func TestCommonReplyRulesInAllTurnPrompts(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	// A non-keyword message queued before the @ turn should appear as
	// "他人对话" in the direct prompt's supplementary context.
	o.Decide(msg("qq:g:1", "B", "今天天气不错", false))
	direct := o.Decide(msg("qq:g:1", "A", "推荐几个好玩的游戏", true))
	if !direct.Forward {
		t.Fatal("at message should forward")
	}
	batch := o.Decide(msg("qq:g:1", "A", "你们觉得什么游戏好玩", false))
	if !batch.Forward {
		t.Fatal("keyword message should forward as a batch")
	}

	common := []string{
		"口语化，不要列条陈词、不要像客服",
		"思考过程不要写进回复，只输出最终结论",
		"回复不同的人或不同主体",
		"get_msg",
		"RULES_SAFE",
		"解答各种问题（生活、游戏",
		"炒股都可以",
		"codex 配置中的真实模型版本",
		"按 DeepSeek 系如实回答",
		"补充近期对话",
		"你的近期回复",
	}
	for _, p := range []struct{ name, content string }{{"direct", direct.Content}, {"batch", batch.Content}} {
		for _, want := range common {
			if !strings.Contains(p.content, want) {
				t.Fatalf("%s prompt missing common rule %q", p.name, want)
			}
		}
		for _, forbidden := range []string{"1-2 句", "不加前缀或引用标记", "Codex 包装层", "不在此写死", "数码", "本次触发", "补充上下文", "回复要求", "只用于分隔多条最终回复", "不要在回复前写判断理由"} {
			if strings.Contains(p.content, forbidden) {
				t.Fatalf("%s prompt should not contain %q", p.name, forbidden)
			}
		}
	}
	if !strings.Contains(direct.Content, "他人对话") {
		t.Fatal("direct prompt should carry queued messages under 他人对话")
	}
	if !strings.Contains(direct.Content, "最新消息") {
		t.Fatal("direct prompt should mark the @ trigger as 最新消息")
	}
	if !strings.Contains(batch.Content, "待处理消息") {
		t.Fatal("batch prompt should carry a marked candidate section")
	}
	if !strings.Contains(batch.Content, "最值得回复的前 20%") {
		t.Fatal("batch prompt should carry the configured top-X% participation rule")
	}
}

func TestExtraRulesAppendAfterCommonRules(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)
	o.cfg.GroupPrompts[promptBlockExtraRules] = "- EXTRA-RULE\n"

	d := o.Decide(msg("qq:g:1", "A", "在吗", true))
	if !d.Forward {
		t.Fatal("at message should forward")
	}
	commonAt := strings.Index(d.Content, "口语化，不要列条陈词")
	extraAt := strings.Index(d.Content, "EXTRA-RULE")
	if commonAt < 0 || extraAt < commonAt {
		t.Fatalf("extra rules should follow common rules: %+v", d.Content)
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
	if !d.Forward || d.Effort != "medium" || !d.SuppressProgress || !d.SplitReplies || !d.Direct {
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
	o := newTestObserverWithWindow(cfg)
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
	o := newTestObserverWithWindow(cfg)

	// @ 高优回合，随后机器人真实回复。
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
		t.Fatal("engage 追问不应被描述成 @ 高优回合")
	}
}

func TestEngageWindowAfterAliasReplyForwardsFollowUp(t *testing.T) {
	cfg := defaultConfig()
	cfg.WindowSec = 600
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.PersonaAlias = "小鲸鱼"
	o := newTestObserverWithWindow(cfg)

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
	o := newTestObserverWithWindow(cfg)

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

func TestNonActivatingPendingRidesAlongLaterBatch(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	// M1/M2 have no activation: no turn yet, but they stay queued.
	if d := o.Decide(msg("qq:g:1", "A", "闲聊内容甲", false)); d.Forward {
		t.Fatal("M1 should not forward on its own")
	}
	if d := o.Decide(msg("qq:g:1", "B", "闲聊内容乙", false)); d.Forward {
		t.Fatal("M2 should not forward on its own")
	}
	// M3 triggers via keyword: accumulated M1/M2 must ride along.
	d3 := o.Decide(msg("qq:g:1", "C", "你们怎么看这个话题", false))
	if !d3.Forward {
		t.Fatal("M3 should forward")
	}
	for _, want := range []string{"闲聊内容甲", "闲聊内容乙", "你们怎么看这个话题"} {
		if !strings.Contains(d3.Content, want) {
			t.Fatalf("batch missing accumulated message %q", want)
		}
	}
}

func TestBatchExcludesStalePending(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.WindowSec = 600
	o := newTestObserverWithWindow(cfg)
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
	o := newTestObserverWithWindow(cfg)

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
	o := newTestObserverWithWindow(cfg)
	o.SetSessionBusy(func(string) bool { return true })

	d := o.Decide(msg("qq:g:1", "A", "你们觉得什么好玩", false))
	if !d.Pending || d.Forward {
		t.Fatalf("busy non-@ message should pend: %+v", d)
	}
}

func TestAtCarriesPendingCatchupAndConsumes(t *testing.T) {
	cfg := defaultConfig()
	cfg.WindowSec = 600
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserverWithWindow(cfg)

	// a: non-triggering message stays queued.
	if d := o.Decide(msg("qq:g:1", "A", "闲聊背景甲", false)); d.Forward {
		t.Fatal("a should not forward on its own")
	}
	// b: @ 触发高优回合，携带 a 作为背景并标记 @。
	d := o.Decide(msg("qq:g:1", "B", "b直接问问题", true))
	if !d.Forward {
		t.Fatal("b should forward")
	}
	if !strings.Contains(d.Content, "被 @ 的直接对话") || !strings.Contains(d.Content, "b直接问问题") {
		t.Fatal("at trigger missing from prompt")
	}
	if !strings.Contains(d.Content, "他人对话") || !strings.Contains(d.Content, "闲聊背景甲") {
		t.Fatalf("at turn should carry cached background under 他人对话: %+v", d.Content)
	}
	o.mu.Lock()
	pending := len(o.groups["qq:g:1"].pending)
	st := o.groups["qq:g:1"]
	for i := range st.replies {
		st.replies[i].At = time.Now().Add(-11 * time.Minute) // 关闭 engage 窗口
	}
	o.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending should be consumed by the at turn, pending=%d", pending)
	}
	// c stays queued; e (keyword) rides along with c but not with the consumed a.
	if d := o.Decide(msg("qq:g:1", "C", "闲聊背景丙", false)); d.Forward {
		t.Fatal("c should not forward on its own")
	}
	d2 := o.Decide(msg("qq:g:1", "E", "你们怎么看", false))
	if !d2.Forward || !strings.Contains(d2.Content, "闲聊背景丙") {
		t.Fatalf("later batch should carry queued c: %+v", d2.Content)
	}
	if strings.Contains(d2.Content, "闲聊背景甲") {
		t.Fatal("consumed a should not be re-carried in a later batch")
	}
}

func TestMultiUserCooldownReusesRuleWindow(t *testing.T) {
	cfg := defaultConfig()
	cfg.WindowSec = 600
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.Rules = []Rule{{Type: "multi_user", MinUsers: 2, WindowSec: 300}}
	o := newTestObserverWithWindow(cfg)

	if d := o.Decide(msg("qq:g:1", "A", "闲聊甲", false)); d.Forward {
		t.Fatal("single sender should not trigger multi_user")
	}
	if d := o.Decide(msg("qq:g:1", "B", "闲聊乙", false)); !d.Forward {
		t.Fatal("two senders should trigger multi_user")
	}
	// Third sender within the same window: cooldown blocks a re-trigger.
	if d := o.Decide(msg("qq:g:1", "C", "闲聊丙", false)); d.Forward {
		t.Fatal("multi_user should not re-trigger within its window")
	}
	// Simulate the window expiring: triggers again.
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	st.lastMultiUserAt = time.Now().Add(-301 * time.Second)
	o.mu.Unlock()
	if d := o.Decide(msg("qq:g:1", "D", "闲聊丁", false)); !d.Forward {
		t.Fatal("multi_user should trigger again after its window")
	}
}

func TestDailyBatchResetRunsOncePerGroupPerDay(t *testing.T) {
	cfg := defaultConfig()
	cfg.ResetHour = 4
	cfg.ResetContextThreshold = 200000
	o := newTestObserver(cfg)
	var calls []string
	o.SetSessionResetter(func(sessionKey string, maxTokens int) bool {
		calls = append(calls, sessionKey)
		return true
	})
	// 群会话先出现（消息时间在重置窗口外也可）。
	// 播种群会话（时间任意，仅让 groups 里有记录）。
	o.Decide(msgAt("qq:g:1", "A", "消息", false, "2026-08-09T03:00:00"))
	o.Decide(msgAt("qq:g:2", "B", "消息", false, "2026-08-09T03:01:00"))

	// 4 点窗口内批量检查：每群一次。
	o.dailyBatchReset(mustLocal("2026-08-10T04:00:30"))
	if len(calls) != 2 {
		t.Fatalf("batch should check each group once, calls=%v", calls)
	}
	// 同一窗口再次执行：不重复。
	o.dailyBatchReset(mustLocal("2026-08-10T04:15:00"))
	if len(calls) != 2 {
		t.Fatalf("batch should not re-check same day, calls=%v", calls)
	}
	// 非重置窗口：不执行。
	o.dailyBatchReset(mustLocal("2026-08-10T09:00:00"))
	if len(calls) != 2 {
		t.Fatalf("batch should skip outside reset hour, calls=%v", calls)
	}
	// 第二天：再次批量检查。
	o.dailyBatchReset(mustLocal("2026-08-11T04:00:30"))
	if len(calls) != 4 {
		t.Fatalf("batch should check again next day, calls=%v", calls)
	}
}

func TestNextResetTime(t *testing.T) {
	cfg := defaultConfig()
	cfg.ResetHour = 4
	o := newTestObserver(cfg)
	if got := o.nextResetTime(mustLocal("2026-08-10T03:00:00")); !got.Equal(mustLocal("2026-08-10T04:00:00")) {
		t.Fatalf("before 4am should target today 4am, got %v", got)
	}
	if got := o.nextResetTime(mustLocal("2026-08-10T04:00:00")); !got.Equal(mustLocal("2026-08-11T04:00:00")) {
		t.Fatalf("at 4am exactly should target tomorrow, got %v", got)
	}
	if got := o.nextResetTime(mustLocal("2026-08-10T23:59:00")); !got.Equal(mustLocal("2026-08-11T04:00:00")) {
		t.Fatalf("after 4am should target next day, got %v", got)
	}
}

func TestDailyResetDisabledWithoutThreshold(t *testing.T) {
	cfg := defaultConfig()
	cfg.ResetHour = 4
	cfg.ResetContextThreshold = 0 // 未启用
	o := newTestObserver(cfg)
	calls := 0
	o.SetSessionResetter(func(string, int) bool { calls++; return true })
	o.dailyBatchReset(mustLocal("2026-08-10T04:00:00"))
	o.Start() // 未启用时 no-op，不应 panic
	o.Close()
	if calls != 0 {
		t.Fatalf("threshold 0 should disable daily reset, calls=%d", calls)
	}
}

func TestNoReplyThrottleWindowCountAndReset(t *testing.T) {
	cfg := defaultConfig()
	cfg.NoReplyThrottleMin = 2
	cfg.WindowSec = 600
	cfg.NoReplyThrottleGap = 120
	o := newTestObserverWithWindow(cfg)
	o.Decide(msg("qq:g:1", "A", "闲聊", false)) // 建立群状态

	o.mu.Lock()
	st := o.groups["qq:g:1"]
	t0 := time.Now()
	o.recordNoReplyLocked(st, t0)
	o.recordNoReplyLocked(st, t0.Add(time.Minute))
	// 计数达阈值后第一次转交放行并记录间隔。
	if !o.noReplyThrottleAllowsLocked(st, t0.Add(90*time.Second)) {
		t.Fatal("first throttled batch should be allowed")
	}
	// 间隔未满 2min：不放行。
	if o.noReplyThrottleAllowsLocked(st, t0.Add(95*time.Second)) {
		t.Fatal("should hold within the 2min gap")
	}
	// 间隔已过：放行。
	if !o.noReplyThrottleAllowsLocked(st, t0.Add(4*time.Minute)) {
		t.Fatal("should allow after the 2min gap")
	}
	// 窗口剪枝：超过 10min 的 NO_REPLY 自动出窗。
	st.noReplyTimes = nil
	o.recordNoReplyLocked(st, t0)
	o.recordNoReplyLocked(st, t0.Add(30*time.Second))
	o.pruneNoReplyLocked(st, t0.Add(11*time.Minute))
	if len(st.noReplyTimes) != 0 {
		t.Fatalf("window aging should drop old entries, got %d", len(st.noReplyTimes))
	}
	o.mu.Unlock()

	// 真实回复重置计数。
	o.mu.Lock()
	o.recordNoReplyLocked(st, time.Now())
	o.mu.Unlock()
	o.OnGroupReply("qq:g:1")
	o.mu.Lock()
	if len(o.groups["qq:g:1"].noReplyTimes) != 0 || !o.groups["qq:g:1"].lastNoReplyTurn.IsZero() {
		t.Fatal("real reply should reset throttle state")
	}
	o.mu.Unlock()
}

func TestNoReplyThrottleHoldsProactiveBatchAndKeepsPending(t *testing.T) {
	cfg := defaultConfig()
	cfg.NoReplyThrottleMin = 2
	cfg.WindowSec = 600
	cfg.NoReplyThrottleGap = 120
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserverWithWindow(cfg)

	o.Decide(msg("qq:g:1", "A", "闲聊", false))
	o.OnSilentTurn("qq:g:1")
	o.OnSilentTurn("qq:g:1")

	// 达阈值后第一条关键词消息：放行（降频批次开始计时）。
	if d := o.Decide(msg("qq:g:1", "B", "你们怎么看这个", false)); !d.Forward {
		t.Fatal("first batch after throttle should forward")
	}
	// 2min 内的第二条：hold，pending 保留。
	if d := o.Decide(msg("qq:g:1", "C", "有没有更好的", false)); d.Forward {
		t.Fatal("within the 2min gap the batch should be held")
	}
	// 模拟 2min 间隔已过：下一条消息放行，且把攒住的 C 一起带上。
	o.mu.Lock()
	o.groups["qq:g:1"].lastNoReplyTurn = time.Now().Add(-121 * time.Second)
	o.mu.Unlock()
	d := o.Decide(msg("qq:g:1", "E", "怎么操作", false))
	if !d.Forward {
		t.Fatal("after the gap the batch should forward")
	}
	if !strings.Contains(d.Content, "有没有更好的") {
		t.Fatalf("held pending message should ride along: %.80s", d.Content)
	}
	// @ 高优消息不受降频影响（即便计数仍达标、间隔未到）。
	if d := o.Decide(msg("qq:g:1", "D", "在吗", true)); !d.Forward {
		t.Fatal("at messages must bypass the throttle")
	}
}

func TestAtNotPendingWhenBusy(t *testing.T) {
	cfg := defaultConfig()
	o := newTestObserverWithWindow(cfg)
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
	o := newTestObserverWithWindow(cfg)
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
	o := newTestObserverWithWindow(cfg)
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
	o := newTestObserverWithWindow(cfg)
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
	o := newTestObserverWithWindow(cfg)
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
	o := newTestObserverWithWindow(cfg)
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
	o := newTestObserverWithWindow(cfg)
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
	candStart := strings.Index(forwarded[0].Content, "【待处理消息】")
	candEnd := strings.Index(forwarded[0].Content, "【补充近期对话】")
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

func TestBatchCandidatesBackgroundNoDuplicateNoLoss(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	cfg.MaxBatchMsgs = 2
	o := newTestObserver(cfg)

	// 前三条非关键词排队；第四条关键词触发批量，max_batch_msgs=2 只保留
	// 最新的两条作候选；候选与背景分属两个区，每条恰好出现一次
	// （不重复、不丢失）。
	o.Decide(msgAt("qq:g:1", "A", "话题一随便聊聊", false, "2026-08-15T14:00:00"))
	o.Decide(msgAt("qq:g:1", "B", "话题二随便聊聊", false, "2026-08-15T14:00:05"))
	o.Decide(msgAt("qq:g:1", "C", "话题三随便聊聊", false, "2026-08-15T14:00:10"))
	d := o.Decide(msgAt("qq:g:1", "D", "话题四你们怎么看", false, "2026-08-15T14:00:15"))
	if !d.Forward {
		t.Fatal("keyword message should forward as a batch")
	}
	content := d.Content
	candStart := strings.Index(content, "【待处理消息】")
	candEnd := strings.Index(content, "【补充近期对话】")
	if candStart < 0 || candEnd < candStart {
		t.Fatalf("batch prompt should keep separate candidate/background sections: %+v", content)
	}
	candSection := content[candStart:candEnd]
	for _, s := range []string{"话题一", "话题二", "话题三", "话题四"} {
		if n := strings.Count(content, s); n != 1 {
			t.Fatalf("message %q should appear exactly once, count=%d", s, n)
		}
	}
	for _, s := range []string{"话题三", "话题四"} {
		if !strings.Contains(candSection, s) {
			t.Fatalf("candidate section missing %q", s)
		}
	}
	for _, s := range []string{"话题一", "话题二"} {
		if strings.Contains(candSection, s) {
			t.Fatalf("over-cap message %q should not be a candidate", s)
		}
	}
}

func TestNonActivatingPendingStaysQueuedAndRidesAlong(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserver(cfg)

	// Non-keyword message: no turn, but it stays queued for a later batch.
	if d := o.Decide(msg("qq:g:1", "A", "今天天气不错", false)); d.Forward {
		t.Fatal("non-keyword message should not forward")
	}
	o.mu.Lock()
	st := o.groups["qq:g:1"]
	pending := len(st.pending)
	bufLen := len(st.buf)
	o.mu.Unlock()
	if pending != 1 {
		t.Fatalf("non-activating message should stay queued, pending=%d", pending)
	}
	if bufLen == 0 {
		t.Fatal("non-activating message should stay in the rolling buffer as context")
	}

	// A later keyword batch should carry it as a batch candidate.
	d := o.Decide(msg("qq:g:1", "B", "你们怎么看这个", false))
	if !d.Forward {
		t.Fatal("keyword message should forward as a batch")
	}
	candStart := strings.Index(d.Content, "【待处理消息】")
	candEnd := strings.Index(d.Content, "【补充近期对话】")
	if candEnd < 0 {
		candEnd = len(d.Content)
	}
	if candStart < 0 || candEnd < candStart || !strings.Contains(d.Content[candStart:candEnd], "今天天气不错") {
		t.Fatalf("earlier queued message should ride along as a batch candidate: %+v", d.Content)
	}
}

func TestBatchPromptCarriesSenderQQAndNoNamePrefix(t *testing.T) {
	cfg := defaultConfig()
	cfg.CooldownSec = 0
	cfg.MinIntervalSec = 0
	cfg.MaxPerHour = 100
	o := newTestObserverWithWindow(cfg)
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
	for _, forbidden := range []string{"开头点出对方名字", "不要用其他分隔符", "[CQ:at", "最多", "config.toml", ".codex"} {
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
	for _, want := range []string{"[mid=10086]", "〔图片〕", "媒体消息", "不需要判断是否回复"} {
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
	o := newTestObserverWithWindow(cfg)
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
