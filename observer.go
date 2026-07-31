// Package observer implements a passive group-chat observer for cc-connect.
//
// It keeps an in-memory rolling buffer of group messages and, when configurable
// activation rules hit, runs one model turn (in its own session) that decides
// whether to interject with a human-like reply or stay silent (NO_REPLY).
//
// Design goals: minimal intrusion into cc-connect core. cc-connect only wires
// a master switch (config "observer = true") and routes group messages to the
// observer via a small platform hook; all logic and configuration live here.
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
	"github.com/chenhg5/cc-connect/core"
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
	Rules                  []Rule `toml:"rules"`
}

// Rule is one activation rule. v1 implements type "keyword"; the rule list is
// extensible for future types (burst / multi_user / random / ...).
type Rule struct {
	Type  string   `toml:"type"`
	Regex []string `toml:"regex"`
	// Reserved for future rule types:
	// MinUsers, WindowSec, MinCount, Prob
}

// ObservedMessage is one buffered group message.
type ObservedMessage struct {
	At     time.Time
	Sender string
	Text   string
}

// replyEvent records when the bot replied (or was @-triggered) in a group,
// used for per-session frequency control and prompt annotations.
type replyEvent struct {
	At   time.Time
	Kind string // "proactive" | "at"
}

type groupState struct {
	chatName string
	buf      []ObservedMessage
	replies  []replyEvent
	lastAct  time.Time
}

const maxReplyEvents = 30

// Observer is the passive group-chat observer instance (one per engine).
type Observer struct {
	engine *core.Engine
	cfg    Config
	ignore []*regexp.Regexp
	run    func(job *core.CronJob) error

	mu     sync.Mutex
	groups map[string]*groupState
}

// GroupObserverHook is implemented by platforms that can route group messages
// to the observer (e.g. the QQ platform).
type GroupObserverHook interface {
	SetGroupObserver(func(*core.Message))
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
		Rules: []Rule{{
			Type:  "keyword",
			Regex: []string{"为什么", "怎么回事", "你们觉得", "有没有"},
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

// New creates an observer wired to the given engine.
func New(engine *core.Engine, cfg Config) *Observer {
	o := &Observer{
		engine: engine,
		cfg:    cfg,
		groups: map[string]*groupState{},
	}
	if engine != nil {
		o.run = engine.ExecuteCronJob
	}
	return o
}

// SetRunner overrides the execution function (used by tests).
func (o *Observer) SetRunner(fn func(job *core.CronJob) error) {
	o.run = fn
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

// OnGroupMessage is the platform hook: called for every group message (both
// @mentioned and not). It records context, tracks @ replies, and may activate
// an observation turn.
func (o *Observer) OnGroupMessage(msg *core.Message) {
	if msg == nil || strings.TrimSpace(msg.Content) == "" {
		return
	}
	for _, re := range o.ignore {
		if re.MatchString(msg.Content) {
			return
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
	st.buf = append(st.buf, ObservedMessage{
		At:     time.Now(),
		Sender: msg.UserName,
		Text:   truncate(msg.Content, 300),
	})
	if o.cfg.BufferSize > 0 && len(st.buf) > o.cfg.BufferSize {
		st.buf = st.buf[len(st.buf)-o.cfg.BufferSize:]
	}
	if msg.Mentioned {
		st.replies = append(st.replies, replyEvent{At: time.Now(), Kind: "at"})
		pruneReplies(st)
	}
	activate := o.maybeActivate(st, msg.Content)
	o.mu.Unlock()

	if activate && o.run != nil {
		go o.runObservation(msg.SessionKey, st)
	}
}

// maybeActivate applies cooldown, proactive-frequency limits, then the rules.
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
	if !rulesMatch(o.cfg.Rules, content) {
		return false
	}
	st.lastAct = now
	return true
}

// rulesMatch reports whether any configured rule matches the message.
// v1 implements "keyword"; unknown rule types are skipped (reserved for later).
func rulesMatch(rules []Rule, content string) bool {
	for _, r := range rules {
		switch strings.ToLower(strings.TrimSpace(r.Type)) {
		case "", "keyword":
			for _, p := range r.Regex {
				if re, err := regexp.Compile(p); err == nil && re.MatchString(content) {
					return true
				}
			}
		default:
			// reserved: burst / multi_user / random / ...
		}
	}
	return false
}

// runObservation builds the observation prompt and runs one model turn in its
// own fresh session. The model decides to reply or stay silent (NO_REPLY).
func (o *Observer) runObservation(key string, st *groupState) {
	msgs := o.contextMsgs(st)
	job := &core.CronJob{
		ID:               fmt.Sprintf("obs-%d", time.Now().UnixNano()),
		SessionKey:       key,
		Prompt:           o.buildPrompt(st, msgs),
		Description:      "群聊观察",
		SessionMode:      "new_per_run",
		Silent:           boolPtr(true),
		SuppressProgress: true,
		TimeoutMins:      intPtr(5),
	}
	if err := o.run(job); err != nil {
		slog.Warn("observer: observation turn failed", "session", key, "error", err)
		return
	}
	// Conservatively count the activation as a proactive reply (frequency
	// control errs on the side of "fewer proactive messages").
	o.mu.Lock()
	if cur := o.groups[key]; cur != nil {
		cur.replies = append(cur.replies, replyEvent{At: time.Now(), Kind: "proactive"})
		pruneReplies(cur)
	}
	o.mu.Unlock()
}

// contextMsgs returns the most recent buffered messages (up to max_context_msgs).
func (o *Observer) contextMsgs(st *groupState) []ObservedMessage {
	n := o.cfg.MaxContextMsgs
	if n <= 0 || len(st.buf) <= n {
		return st.buf
	}
	out := make([]ObservedMessage, n)
	copy(out, st.buf[len(st.buf)-n:])
	return out
}

// buildPrompt assembles the observation prompt handed to the model.
func (o *Observer) buildPrompt(st *groupState, msgs []ObservedMessage) string {
	var sb strings.Builder
	sb.WriteString("你在一个 QQ 群里，以下是最新的群聊记录（时间 发言人：内容）：\n")
	if st.chatName != "" {
		fmt.Fprintf(&sb, "群名：%s\n", st.chatName)
	}
	if len(msgs) == 0 {
		sb.WriteString("（暂无记录）\n")
	}
	for i, m := range msgs {
		fmt.Fprintf(&sb, "%02d. %s %s：%s\n", i+1, m.At.Format("15:04"), m.Sender, m.Text)
	}

	sb.WriteString("\n你近期的发言/触发记录（注意频控，避免高频刷屏）：\n")
	sb.WriteString(o.recentRepliesText(st))

	sb.WriteString(`
请像群里的真人一样决定是否插话：
- 只在有值得接的话头、你真的有观点或能自然融入时才回复；否则保持沉默；
- 回复要口语化、简短（一般 1-2 句），不要列条陈词、不要像客服；
- 不参与时只输出 NO_REPLY，不要发任何其他内容；
- 参与时直接输出要发到群里的内容，不要加前缀或引用标记；
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

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }
