// Package observer implements the "shadow persona" (影子人格) decision layer
// for cc-connect group chats.
//
// cc-connect hosts the codex sessions; this plugin only decides, for every
// group message, whether it is forwarded to the agent and with what parameters
// (prompt content, reasoning effort, progress suppression). It also keeps a
// rolling buffer so shadow turns can supplement the messages that were skipped
// between the last real forwarding point and the current trigger.
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
	ReplyRate              int    `toml:"reply_rate"`               // 0-100: participate only in the top X% of topics worth joining
	WindowSec              int    `toml:"window_sec"`               // unified window: catch-up filter, pending stale-drop, engage window
	AtEffort               string `toml:"at_effort"`                // reasoning effort for @-triggered turns (default "high")
	ShadowEffort           string `toml:"shadow_effort"`            // reasoning effort for shadow turns (default "low")
	SuppressShadowProgress bool   `toml:"suppress_shadow_progress"` // hide thinking/progress for shadow turns
	PersonaName            string `toml:"persona_name"`             // persona display name; empty uses a generic label
	CatchupMaxMsgs         int    `toml:"catchup_max_msgs"`         // max skipped messages to supplement
	CatchupMaxChars        int    `toml:"catchup_max_chars"`        // total supplement text cap (drop oldest beyond this)
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
	Kind string // "proactive" (real shadow reply) | "at" (@-triggered main reply)
}

type groupState struct {
	chatName        string
	buf             []ObservedMessage
	replies         []replyEvent
	pending         []pendingMsg // shadow candidates awaiting the in-flight turn
	lastAct         time.Time
	lastForwardedAt time.Time // newest message actually forwarded to the agent
}

// pendingMsg is a shadow candidate deferred until the running turn completes.
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
		AtEffort:               "high",
		ShadowEffort:           "low",
		SuppressShadowProgress: true,
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
// running, non-@ shadow decisions are deferred until it completes.
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
// @-bot messages are always forwarded (main persona, at_effort); other
// messages are forwarded as shadow turns when the activation rules hit.
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

	if msg.Mentioned {
		st.replies = append(st.replies, replyEvent{At: entry.At, Kind: "at"})
		pruneReplies(st)
		st.lastForwardedAt = entry.At
		o.mu.Unlock()
		slog.Info("observer: decision", "session", msg.SessionKey, "mentioned", true, "outcome", "forward")
		return Decision{Forward: true, Content: msg.Content, Effort: effortOr(o.cfg.AtEffort, "high")}
	}

	// Non-@ candidate. If a turn is in flight, defer the decision until it
	// completes: the running task changes the last-effective-reply time, which
	// the shadow decision depends on. (@ messages are decided immediately and
	// rely on cc-connect's own queueing.)
	if o.busy != nil && o.busy(msg.SessionKey) {
		st.pending = append(st.pending, pendingMsg{msg: msg, entry: entry})
		o.mu.Unlock()
		slog.Info("observer: decision", "session", msg.SessionKey, "busy", true, "outcome", "pending")
		return Decision{Pending: true}
	}

	// Flush deferred decisions from earlier busy turns (e.g. turns that ended
	// without a delivered reply) before deciding the current message.
	pendingDecisions, requeue := o.processPendingLocked(st)
	st.pending = requeue
	cur := o.decideShadowLocked(st, pendingMsg{msg: msg, entry: entry})
	o.mu.Unlock()

	for _, d := range pendingDecisions {
		if o.forward != nil {
			if err := o.forward(msg.SessionKey, d); err != nil {
				slog.Warn("observer: forward deferred decision failed", "session", msg.SessionKey, "error", err)
			}
		}
	}
	outcome := "drop"
	if cur.Forward {
		outcome = "forward"
	}
	slog.Info("observer: decision", "session", msg.SessionKey, "busy", false, "outcome", outcome)
	return cur
}

// decideShadowLocked decides a shadow candidate against the current state.
// The caller must hold o.mu.
func (o *Observer) decideShadowLocked(st *groupState, p pendingMsg) Decision {
	if !o.maybeActivate(st, p.msg.Content) {
		return Decision{}
	}
	catchup := o.catchUpLocked(st, p.entry.At)
	prompt := o.buildShadowPrompt(st, p.entry, catchup)
	st.lastForwardedAt = p.entry.At
	return Decision{
		Forward:          true,
		Content:          prompt,
		Effort:           effortOr(o.cfg.ShadowEffort, "low"),
		SuppressProgress: o.cfg.SuppressShadowProgress,
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

	// Re-decide deferred shadow candidates now that the turn has completed.
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

// processPendingLocked evaluates queued shadow candidates in order: stale ones
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
		if d := o.decideShadowLocked(st, p); d.Forward {
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

// buildShadowPrompt assembles the prompt for a shadow turn: persona, the
// triggering message, supplemented skipped context, recent reply records and
// the participation rules.
func (o *Observer) buildShadowPrompt(st *groupState, trigger ObservedMessage, catchup []ObservedMessage) string {
	var sb strings.Builder
	persona := o.cfg.PersonaName
	if strings.TrimSpace(persona) == "" {
		persona = "本群机器人"
	}
	sb.WriteString(`【你的身份与背景】

你是这个群里“` + persona + `”的影子人格：主人格（主会话）负责被 @ 时的正式答疑，而你作为影子一直
暗中随行、围观群聊，平时像熟朋友一样爱接梗、喜欢帮大家解答各种问题（游戏、生活、数码、
见闻都可以），有自己的态度，不谄媚、不机械。

- 主人格不知道你的存在；你始终暗中随行，与他表现一致——不重复他刚说过的话，
  不推翻他的结论（除非确有明显错误），顺着他的话补充；
- 分工：主人格处理被 @ 时的正式、深度回答；你负责主动观察、补一句、接住话头、活跃气氛；
- 对外（群里）你就是` + persona + `本人：不透露存在“影子人格/主会话/双人格”之分，也不要说自己是 AI 分身。

你习惯围观多于插话——大多数时候安静待在群里，遇到值得说的话题才开口。

【本次触发】

你在群里被触发参与，触发你的消息：
`)
	fmt.Fprintf(&sb, "%s %s：%s\n", trigger.At.Format("15:04"), trigger.Sender, trigger.Text)
	if len(catchup) > 0 {
		sb.WriteString("\n【补充上下文（此前未转交的最近消息）】\n")
		for i, m := range catchup {
			fmt.Fprintf(&sb, "%02d. %s %s：%s\n", i+1, m.At.Format("15:04"), m.Sender, m.Text)
		}
	}

	sb.WriteString("\n你近期的发言/触发记录（注意频控，避免高频刷屏）：\n")
	sb.WriteString(o.recentRepliesText(st))

	sb.WriteString(`
请像群里的真人一样决定是否插话：
- 只在有值得接的话头、你真的有观点或能自然融入时才回复；否则保持沉默；
`)
	fmt.Fprintf(&sb, "- 从“最值得回复”的角度自我评价，只参与最值得回复的前 %d%% 的话题（不硬凑、不勉强）；\n", replyRate(o.cfg))
	sb.WriteString(`- 回复要口语化、简短（一般 1-2 句），不要列条陈词、不要像客服；
- 不参与时只输出 NO_REPLY，不要发任何其他内容；
- 参与时直接输出要发到群里的内容，不要加前缀或引用标记；
- 思考过程不要写进回复，只输出最终结论；
- 遵守 RULES_SAFE 群聊安全规则，不暴露任何本机路径、配置、IP 等隐私信息。
`)
	return sb.String()
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
		if r.Kind == "at" {
			kind = "@回复"
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
