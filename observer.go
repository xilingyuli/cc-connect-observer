// Package observer implements the group-chat passive-participation decision
// layer (被动插话) for cc-connect group chats.
//
// cc-connect hosts the codex sessions; this plugin only decides, for every
// group message, whether it is forwarded to the agent and with what parameters
// (prompt content, reasoning effort, progress suppression). It also keeps a
// rolling buffer so proactive turns can supplement the messages that were
// skipped between the last real forwarding point and the current trigger.
package observer

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the observer's own configuration (separate file, e.g.
// ~/.cc-connect/observer.toml).
type Config struct {
	BufferSize             int    `toml:"buffer_size"`
	MaxContextMsgs         int    `toml:"max_context_msgs"`
	CooldownSec            int    `toml:"cooldown_sec"`
	MinIntervalSec         int    `toml:"min_interval_sec"`
	MaxPerHour             int    `toml:"max_per_hour"`
	RecentRepliesForPrompt int    `toml:"recent_replies_for_prompt"`
	ReplyRate              int    `toml:"reply_rate"`              // 0-100: participate only in the top X% of topics worth joining
	WindowSec              int    `toml:"window_sec"`              // unified window: catch-up filter, pending stale-drop, engage window
	AtEffort               string `toml:"at_effort"`               // reasoning effort for direct turns (@ / nickname), default "medium"
	ProactiveEffort        string `toml:"proactive_effort"`        // reasoning effort for proactive turns (default "low")
	SuppressGroupProgress  bool   `toml:"suppress_group_progress"` // hide thinking/progress for all group turns
	PersonaName            string `toml:"persona_name"`            // persona display name; empty uses a generic label
	PersonaAlias           string `toml:"persona_alias"`           // alias/nickname; mentioned in the persona when set
	CatchupMaxMsgs         int    `toml:"catchup_max_msgs"`        // max skipped messages to supplement
	CatchupMaxChars        int    `toml:"catchup_max_chars"`       // total supplement text cap (drop oldest beyond this)
	Rules                  []Rule `toml:"rules"`
}

// Rule is one activation rule. v1 implements type "keyword"; the rule list is
// extensible for future types (burst / multi_user / random / ...).
type Rule struct {
	Type      string   `toml:"type"`
	Regex     []string `toml:"regex"`
	MinUsers  int      `toml:"min_users"`  // multi_user: distinct participants required
	WindowSec int      `toml:"window_sec"` // multi_user: lookback window (0 = 300s)
}

// IncomingMessage is the minimal message view the observer needs from the
// platform (the host adapts its own message type into this one).
type IncomingMessage struct {
	SessionKey string
	UserName   string
	ChatName   string
	Content    string
	Mentioned  bool
}

// Decision is the forwarding decision the host executes.
type Decision struct {
	Forward          bool
	Pending          bool   // deferred: decide after the in-flight turn completes
	Content          string // prompt/content to send; empty keeps the original
	Effort           string // reasoning effort override for this turn
	SuppressProgress bool   // hide thinking/tool progress for this turn
}

// ObservedMessage is one buffered group message (text only).
type ObservedMessage struct {
	At     time.Time
	Sender string
	Text   string
}

// replyEvent records when the bot replied (or was @-triggered) in a group,
// used for per-session frequency control and prompt annotations.
type replyEvent struct {
	At   time.Time
	Kind string // "proactive" (real proactive reply) | "at" (@-directed) | "alias" (nickname-directed)
}

type groupState struct {
	chatName        string
	buf             []ObservedMessage
	replies         []replyEvent
	pending         []pendingMsg // proactive candidates awaiting the in-flight turn
	lastAct         time.Time
	lastForwardedAt time.Time // newest message actually forwarded to the agent
}

// pendingMsg is a proactive candidate deferred until the running turn completes.
type pendingMsg struct {
	msg   IncomingMessage
	entry ObservedMessage
}

const maxReplyEvents = 30

// Observer is the decision layer (one per engine/project).
type Observer struct {
	cfg     Config
	ignore  []*regexp.Regexp
	busy    func(sessionKey string) bool
	forward func(sessionKey string, d Decision) error

	mu     sync.Mutex
	groups map[string]*groupState
}

// defaultConfig returns the built-in defaults; a config file can override them.
func defaultConfig() Config {
	return Config{
		BufferSize:             50,
		MaxContextMsgs:         30,
		CooldownSec:            600,
		MinIntervalSec:         900,
		MaxPerHour:             3,
		RecentRepliesForPrompt: 10,
		ReplyRate:              20,
		WindowSec:              600,
		AtEffort:               "medium",
		ProactiveEffort:        "low",
		SuppressGroupProgress:  true,
		CatchupMaxMsgs:         100,
		CatchupMaxChars:        2000,
		Rules: []Rule{{
			Type:  "keyword",
			Regex: []string{"什么", "怎么", "吗", "呢", "你们", "？"},
		}, {
			Type:      "multi_user",
			MinUsers:  4,
			WindowSec: 300,
		}},
	}
}

// LoadConfig reads the observer config file. A missing file yields defaults.
func LoadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// New creates an observer.
func New(cfg Config) *Observer {
	return &Observer{cfg: cfg, groups: map[string]*groupState{}}
}

// SetIgnorePatterns sets regexes that exclude messages from observation
// (e.g. cc-connect's block_reply_patterns for #/* commands).
func (o *Observer) SetIgnorePatterns(patterns []string) {
	var re []*regexp.Regexp
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if r, err := regexp.Compile(p); err == nil {
			re = append(re, r)
		}
	}
	o.ignore = re
}

// SetSessionBusy registers the host's in-flight-turn check. When a turn is
// running, non-@ proactive decisions are deferred until it completes.
func (o *Observer) SetSessionBusy(fn func(sessionKey string) bool) {
	o.busy = fn
}

// SetForwarder registers the host callback that injects a decided message into
// the agent (used for deferred pending decisions).
func (o *Observer) SetForwarder(fn func(sessionKey string, d Decision) error) {
	o.forward = fn
}

// Decide is the group-message forwarding policy: it records the message,
// updates the forwarding point, and decides whether/how to forward it.
// @-bot messages and explicit nickname mentions are direct conversations and
// always forward with at_effort; other messages are forwarded as proactive
// turns when the activation rules hit.
func (o *Observer) Decide(msg IncomingMessage) Decision {
	if strings.TrimSpace(msg.Content) == "" {
		return Decision{}
	}
	for _, re := range o.ignore {
		if re.MatchString(msg.Content) {
			return Decision{}
		}
	}

	o.mu.Lock()

	st := o.groups[msg.SessionKey]
	if st == nil {
		st = &groupState{}
		o.groups[msg.SessionKey] = st
	}
	if msg.ChatName != "" {
		st.chatName = msg.ChatName
	}
	entry := ObservedMessage{At: time.Now(), Sender: msg.UserName, Text: truncate(msg.Content, 300)}
	st.buf = append(st.buf, entry)
	if o.cfg.BufferSize > 0 && len(st.buf) > o.cfg.BufferSize {
		st.buf = st.buf[len(st.buf)-o.cfg.BufferSize:]
	}

	if msg.Mentioned || o.aliasMentioned(msg.Content) {
		kind := "at"
		if !msg.Mentioned {
			kind = "alias"
		}
		st.replies = append(st.replies, replyEvent{At: entry.At, Kind: kind})
		pruneReplies(st)
		st.lastForwardedAt = entry.At
		prompt := o.buildTurnPrompt(st, entry, nil, kind)
		o.mu.Unlock()
		slog.Info("observer: decision", "session", msg.SessionKey, "direct", true, "outcome", "forward")
		return Decision{Forward: true, Content: prompt, Effort: effortOr(o.cfg.AtEffort, "medium"), SuppressProgress: o.cfg.SuppressGroupProgress}
	}

	// Non-direct candidate: always enters the pending queue first, then the
	// queue is processed. Busy -> the entry waits for the next busy-cycle;
	// free -> "enqueue == dequeue" (processed immediately). Forwarding is
	// strictly serial: after one entry is forwarded (which injects a turn),
	// the rest of the queue waits. Direct messages are decided immediately
	// and rely on cc-connect's own queueing.
	st.pending = append(st.pending, pendingMsg{msg: msg, entry: entry})
	if o.busy != nil && o.busy(msg.SessionKey) {
		o.mu.Unlock()
		slog.Info("observer: decision", "session", msg.SessionKey, "busy", true, "outcome", "pending")
		return Decision{Pending: true}
	}

	decisions, requeue := o.processPendingLocked(st)
	st.pending = requeue
	o.mu.Unlock()

	if len(decisions) == 0 {
		slog.Info("observer: decision", "session", msg.SessionKey, "busy", false, "outcome", "drop")
		return Decision{}
	}
	if len(requeue) == 0 {
		// The current (newest) entry is the one forwarded -> the platform
		// forwards it now.
		slog.Info("observer: decision", "session", msg.SessionKey, "busy", false, "outcome", "forward")
		return decisions[0]
	}
	// An earlier entry was forwarded (injecting a turn); the current stays
	// queued and older decisions are delivered via the forwarder.
	for _, d := range decisions {
		if o.forward != nil {
			if err := o.forward(msg.SessionKey, d); err != nil {
				slog.Warn("observer: forward deferred decision failed", "session", msg.SessionKey, "error", err)
			}
		}
	}
	slog.Info("observer: decision", "session", msg.SessionKey, "busy", false, "outcome", "pending")
	return Decision{Pending: true}
}

// decideProactiveLocked decides a proactive candidate against the current state.
// The caller must hold o.mu.
func (o *Observer) decideProactiveLocked(st *groupState, p pendingMsg) Decision {
	if !o.maybeActivate(st, p.msg.Content) {
		return Decision{}
	}
	catchup := o.catchUpLocked(st, p.entry.At)
	prompt := o.buildTurnPrompt(st, p.entry, catchup, "")
	st.lastForwardedAt = p.entry.At
	return Decision{
		Forward:          true,
		Content:          prompt,
		Effort:           effortOr(o.cfg.ProactiveEffort, "low"),
		SuppressProgress: o.cfg.SuppressGroupProgress,
	}
}

// OnGroupReply records a real reply actually delivered in a group (reported by
// the host via the message.sent lifecycle hook). Used for engage-window and
// frequency tracking; NO_REPLY turns never reach here.
func (o *Observer) OnGroupReply(sessionKey string) {
	o.mu.Lock()
	st := o.groups[sessionKey]
	if st == nil {
		st = &groupState{}
		o.groups[sessionKey] = st
	}
	st.replies = append(st.replies, replyEvent{At: time.Now(), Kind: "proactive"})
	pruneReplies(st)
	pending := st.pending
	st.pending = nil
	o.mu.Unlock()

	// Re-decide deferred proactive candidates now that the turn has completed.
	// If the session is busy again (another turn started), they keep waiting.
	o.mu.Lock()
	cur := o.groups[sessionKey]
	if cur == nil || (o.busy != nil && o.busy(sessionKey)) {
		if cur != nil {
			cur.pending = append(cur.pending, pending...)
		}
		o.mu.Unlock()
		return
	}
	cur.pending = pending
	decisions, requeue := o.processPendingLocked(cur)
	cur.pending = requeue
	o.mu.Unlock()

	for _, d := range decisions {
		if o.forward != nil {
			if err := o.forward(sessionKey, d); err != nil {
				slog.Warn("observer: forward deferred decision failed", "session", sessionKey, "error", err)
			}
		}
	}
}

// processPendingLocked evaluates queued proactive candidates in order: stale ones
// (older than the catchup window) are dropped, non-activating ones are dropped,
// and processing stops after the first forward so later candidates wait for the
// next turn completion. Returns decisions to forward and candidates to re-queue.
// The caller must hold o.mu.
func (o *Observer) processPendingLocked(st *groupState) (decisions []Decision, requeue []pendingMsg) {
	forwardIdx := -1
	for i, p := range st.pending {
		if o.isStale(p.entry.At) {
			continue // arrived too long ago; not worth replying now
		}
		if d := o.decideProactiveLocked(st, p); d.Forward {
			decisions = append(decisions, d)
			forwardIdx = i
			break
		}
	}
	if forwardIdx >= 0 && forwardIdx+1 < len(st.pending) {
		requeue = append(requeue, st.pending[forwardIdx+1:]...)
	}
	return decisions, requeue
}

// isStale reports whether a buffered/pending message is too old to reply to,
// using the unified time window (window_sec).
func (o *Observer) isStale(at time.Time) bool {
	w := time.Duration(o.cfg.WindowSec) * time.Second
	return w > 0 && time.Since(at) > w
}

// maybeActivate applies cooldown, proactive-frequency limits, the engage
// window, then the activation rules.
func (o *Observer) maybeActivate(st *groupState, content string) bool {
	now := time.Now()
	if !st.lastAct.IsZero() && now.Sub(st.lastAct) < time.Duration(o.cfg.CooldownSec)*time.Second {
		return false
	}
	if last := lastProactive(st); !last.IsZero() && now.Sub(last) < time.Duration(o.cfg.MinIntervalSec)*time.Second {
		return false
	}
	if o.cfg.MaxPerHour > 0 && proactiveCountInHour(st, now) >= o.cfg.MaxPerHour {
		return false
	}
	// Engage window: within window_sec after a real proactive reply, keep
	// delivering conversation turns even without a keyword match.
	if last := lastProactive(st); !last.IsZero() && now.Sub(last) < time.Duration(o.cfg.WindowSec)*time.Second {
		st.lastAct = now
		return true
	}
	if !o.rulesMatch(st, content, now) {
		return false
	}
	st.lastAct = now
	return true
}

// catchUpLocked returns the messages observed after the last real forwarding
// point and before the current trigger: text only, most recent N, within the
// time window, and capped by total characters (older parts dropped first).
func (o *Observer) catchUpLocked(st *groupState, now time.Time) []ObservedMessage {
	window := time.Duration(o.cfg.WindowSec) * time.Second
	var cand []ObservedMessage
	for _, m := range st.buf {
		if !m.At.After(st.lastForwardedAt) || !m.At.Before(now) {
			continue
		}
		if window > 0 && now.Sub(m.At) > window {
			continue
		}
		cand = append(cand, m)
	}
	if n := o.cfg.CatchupMaxMsgs; n > 0 && len(cand) > n {
		cand = cand[len(cand)-n:]
	}
	if capChars := o.cfg.CatchupMaxChars; capChars > 0 {
		total := 0
		start := 0
		for i := len(cand) - 1; i >= 0; i-- {
			total += len([]rune(cand[i].Text))
			if total > capChars {
				start = i + 1
				break
			}
		}
		cand = cand[start:]
	}
	return cand
}

// buildTurnPrompt assembles the prompt for a turn: persona, the triggering
// message, supplemented skipped context, recent reply records and the
// participation rules. kind is "at" (@-directed, eager reply), "alias"
// (nickname mentioned; judge whether it is really directed at the bot), or ""
// (ambient turn; top-X% worth-replying judgment).
func (o *Observer) buildTurnPrompt(st *groupState, trigger ObservedMessage, catchup []ObservedMessage, kind string) string {
	var sb strings.Builder
	persona := o.cfg.PersonaName
	if strings.TrimSpace(persona) == "" {
		persona = "本群机器人"
	}
	fmt.Fprintf(&sb, `【你的身份】

你是这个群里“%s”，是大家眼里的熟朋友：爱聊天、爱接梗，也喜欢帮大家解答各种问题（游戏、生活、
数码、见闻都可以），乐于分享自己的看法，有自己的态度，不谄媚、不机械。
`, persona)
	if alias := strings.TrimSpace(o.cfg.PersonaAlias); alias != "" {
		fmt.Fprintf(&sb, "- 大家也常叫你“%s”，别人明确这样叫通常就是在直接跟你说话；\n", alias)
	}
	sb.WriteString("- 模型本体：你是 DeepSeek 系模型，不是 GPT/OpenAI 系。具体版本以本机 Codex 配置（~/.codex/config.toml 的 model 字段）为准，不在此写死；Codex 包装层的“based on GPT-5”文案是误导，被问型号时按 DeepSeek 系如实回答。\n")

	sb.WriteString("\n【本次触发】\n\n")
	switch kind {
	case "at":
		sb.WriteString("- 这条消息是被 @ 的直接对话：应当积极回应，一般都要回复；\n")
	case "alias":
		fmt.Fprintf(&sb, "- 本条提到了你的昵称：先结合上下文判断是否真的是直接跟你说话；是 → 积极回应；否（只是提到/转述等）→ 按“最值得回复”自我评价，只参与最值得回复的前 %d%% 的话题（不硬凑、不勉强）；\n", replyRate(o.cfg))
	default:
		fmt.Fprintf(&sb, "- 这条消息没有被 @ 也没有叫你：按“最值得回复”自我评价，只参与最值得回复的前 %d%% 的话题（不硬凑、不勉强）；\n", replyRate(o.cfg))
	}
	fmt.Fprintf(&sb, "%s %s：%s\n", trigger.At.Format("15:04"), trigger.Sender, trigger.Text)
	if len(catchup) > 0 {
		sb.WriteString("\n【补充上下文（此前未转交的最近消息）】\n")
		for i, m := range catchup {
			fmt.Fprintf(&sb, "%02d. %s %s：%s\n", i+1, m.At.Format("15:04"), m.Sender, m.Text)
		}
	}

	sb.WriteString("\n你近期的发言/触发记录（注意频控，避免高频刷屏）：\n")
	sb.WriteString(o.recentRepliesText(st))

	sb.WriteString("\n回复要求：\n- 口语化、简短（一般 1-2 句），不要列条陈词、不要像客服；\n")
	switch kind {
	case "at":
		sb.WriteString("- 这是直接对话：请积极回应、一般都要回复，除非实在无话可说，否则不要输出 NO_REPLY；\n")
	case "alias":
		sb.WriteString("- 判断为直接对话时不要输出 NO_REPLY；判断为并非直接对话时，不值得参与就输出 NO_REPLY；\n")
	default:
		sb.WriteString("- 不值得参与时只输出 NO_REPLY，不要发任何其他内容；\n")
	}
	sb.WriteString(`- 参与时直接输出要发到群里的内容，不要加前缀或引用标记；
- 思考过程不要写进回复，只输出最终结论；
- 遵守 RULES_SAFE 群聊安全规则，不暴露任何本机路径、配置、IP 等隐私信息。
`)
	return sb.String()
}

// aliasMentioned reports whether the message explicitly addresses the bot by
// its configured nickname (persona_alias).
func (o *Observer) aliasMentioned(content string) bool {
	alias := strings.TrimSpace(o.cfg.PersonaAlias)
	return alias != "" && strings.Contains(content, alias)
}

// recentRepliesText formats the most recent reply events for the prompt.
func (o *Observer) recentRepliesText(st *groupState) string {
	replies := st.replies
	n := o.cfg.RecentRepliesForPrompt
	if n > 0 && len(replies) > n {
		replies = replies[len(replies)-n:]
	}
	if len(replies) == 0 {
		return "（无记录）\n"
	}
	var sb strings.Builder
	for _, r := range replies {
		kind := "主动插话"
		switch r.Kind {
		case "at":
			kind = "@回复"
		case "alias":
			kind = "直接回复"
		}
		fmt.Fprintf(&sb, "- %s %s\n", r.At.Format("01-02 15:04"), kind)
	}
	return sb.String()
}

// rulesMatch reports whether any configured rule matches the message/group
// state. v1 implements "keyword"; "multi_user" counts distinct participants in
// the recent window (the bot itself never enters the buffer, so it is never
// counted); other types are reserved for later.
func (o *Observer) rulesMatch(st *groupState, content string, now time.Time) bool {
	for _, r := range o.cfg.Rules {
		switch strings.ToLower(strings.TrimSpace(r.Type)) {
		case "", "keyword":
			for _, p := range r.Regex {
				if re, err := regexp.Compile(p); err == nil && re.MatchString(content) {
					return true
				}
			}
		case "multi_user":
			window := time.Duration(r.WindowSec) * time.Second
			if window <= 0 {
				window = 5 * time.Minute
			}
			senders := map[string]bool{}
			for _, m := range st.buf {
				if now.Sub(m.At) > window {
					continue
				}
				if m.Sender != "" {
					senders[m.Sender] = true
				}
			}
			if len(senders) >= r.MinUsers {
				return true
			}
		default:
			// reserved: burst / multi_user / random / ...
		}
	}
	return false
}

func lastProactive(st *groupState) time.Time {
	for i := len(st.replies) - 1; i >= 0; i-- {
		if st.replies[i].Kind == "proactive" {
			return st.replies[i].At
		}
	}
	return time.Time{}
}

func proactiveCountInHour(st *groupState, now time.Time) int {
	cnt := 0
	for _, r := range st.replies {
		if r.Kind == "proactive" && now.Sub(r.At) < time.Hour {
			cnt++
		}
	}
	return cnt
}

func pruneReplies(st *groupState) {
	if len(st.replies) > maxReplyEvents {
		st.replies = st.replies[len(st.replies)-maxReplyEvents:]
	}
}

func effortOr(cfg, def string) string {
	if strings.TrimSpace(cfg) == "" {
		return def
	}
	return strings.TrimSpace(cfg)
}

func replyRate(cfg Config) int {
	if cfg.ReplyRate <= 0 || cfg.ReplyRate > 100 {
		return 100
	}
	return cfg.ReplyRate
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
