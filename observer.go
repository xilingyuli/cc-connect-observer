// Package observer implements the group-chat passive-participation decision
// layer (被动插话) for cc-connect group chats.
//
// cc-connect hosts the codex sessions; this plugin only decides, for every
// group message, whether it is forwarded to the agent and with what parameters
// (prompt content, reasoning effort, progress suppression). It also keeps a
// pending queue for normal turns (非 @/昵称消息): when a batch is worth a turn,
// ALL queued candidates since the last processing point are merged into ONE
// task so the agent judges in a single pass which of them are worth replying
// to (instead of one forwarding decision per message), and each reply is
// delivered as its own group message. The rolling buffer supplies surrounding
// context for the batch; candidates that did not justify a turn stay queued
// for a later batch (the buffer keeps them as context in the meantime).
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

// Config is the observer's own configuration (separate TOML file beside the
// cc-connect config; the path is set via the host's observer setting).
type Config struct {
	BufferSize             int             `toml:"buffer_size"`
	MaxContextMsgs         int             `toml:"max_context_msgs"`
	CooldownSec            int             `toml:"cooldown_sec"`
	MinIntervalSec         int             `toml:"min_interval_sec"`
	MaxPerHour             int             `toml:"max_per_hour"`
	RecentRepliesForPrompt int             `toml:"recent_replies_for_prompt"`
	ReplyRate              int             `toml:"reply_rate"`                     // 0-100: participate only in the top X% of topics worth joining
	WindowSec              int             `toml:"window_sec"`                     // 共享窗口：pending 超时即弃 + 拼历史消息(补充上下文)收集
	EngageWindowSec        int             `toml:"engage_window_sec"`              // 连续回话窗口（真实回复后的持续投递）；0 = 跟随 window_sec
	ResetHour              int             `toml:"reset_hour"`                     // 每天定时重置的检查小时（北京时间 0-23）
	ResetContextThreshold  int             `toml:"reset_context_threshold"`        // 会话上下文超过该 token 数才在 reset_hour 后重置；0 = 不启用
	NoReplyThrottleMin     int             `toml:"no_reply_throttle_min"`          // 窗口内累计 NO_REPLY 数达该值后普通回合转交降频；0 = 不启用
	NoReplyThrottleGap     int             `toml:"no_reply_throttle_interval_sec"` // 降频后两次转交最小间隔（秒）
	AtEffort               string          `toml:"at_effort"`                      // 高优回合（@/昵称）的推理强度，默认 "medium"
	ProactiveEffort        string          `toml:"proactive_effort"`               // 普通回合（非 @/昵称）的推理强度，默认 "low"
	SuppressGroupProgress  bool            `toml:"suppress_group_progress"`        // hide thinking/progress for all group turns
	PersonaName            string          `toml:"persona_name"`                   // persona display name; empty uses a generic label
	PersonaAlias           string          `toml:"persona_alias"`                  // alias/nickname; mentioned in the persona when set
	MaxBatchMsgs           int             `toml:"max_batch_msgs"`                 // 普通回合一次最多合并的排队消息条数
	Rules                  []Rule          `toml:"rules"`
	Peak                   PeriodOverrides `toml:"peak"` // 高峰时段(北京时间 9-12/14-18)覆盖项；0/空表示沿用谷峰配置
}

// Rule is one activation rule. v1 implements type "keyword"; the rule list is
// extensible for future types (burst / multi_user / random / ...).
type Rule struct {
	Type      string   `toml:"type"`
	Regex     []string `toml:"regex"`
	MinUsers  int      `toml:"min_users"`  // multi_user: distinct participants required
	WindowSec int      `toml:"window_sec"` // multi_user: lookback window (0 = 300s)
}

// PeriodOverrides 是高峰时段相对谷峰(顶层)配置可覆盖的字段。
// 语义：int 字段 >0 才覆盖、string 非空才覆盖、Rules 非空则整体替换。
type PeriodOverrides struct {
	WindowSec              int    `toml:"window_sec"`
	EngageWindowSec        int    `toml:"engage_window_sec"`
	CooldownSec            int    `toml:"cooldown_sec"`
	MinIntervalSec         int    `toml:"min_interval_sec"`
	MaxPerHour             int    `toml:"max_per_hour"`
	RecentRepliesForPrompt int    `toml:"recent_replies_for_prompt"`
	ReplyRate              int    `toml:"reply_rate"`
	MaxBatchMsgs           int    `toml:"max_batch_msgs"`
	AtEffort               string `toml:"at_effort"`
	ProactiveEffort        string `toml:"proactive_effort"`
	Rules                  []Rule `toml:"rules"`
}

// beijingHour 返回北京时间(UTC+8)的整点小时。
func beijingHour(t time.Time) int {
	return t.In(time.FixedZone("CST", 8*3600)).Hour()
}

// inPeakHours 高峰时段：北京时间 09:00-12:00、14:00-18:00（与 DeepSeek 峰谷定价一致）。
func inPeakHours(t time.Time) bool {
	h := beijingHour(t)
	return (h >= 9 && h < 12) || (h >= 14 && h < 18)
}

// effective 返回给定时间点应生效的配置：高峰时段合并 [peak] 覆盖项，否则用顶层(谷峰)配置。
func (c Config) effective(at time.Time) Config {
	if !inPeakHours(at) {
		return c
	}
	p := c.Peak
	if p.WindowSec > 0 {
		c.WindowSec = p.WindowSec
	}
	if p.EngageWindowSec > 0 {
		c.EngageWindowSec = p.EngageWindowSec
	}
	if p.CooldownSec > 0 {
		c.CooldownSec = p.CooldownSec
	}
	if p.MinIntervalSec > 0 {
		c.MinIntervalSec = p.MinIntervalSec
	}
	if p.MaxPerHour > 0 {
		c.MaxPerHour = p.MaxPerHour
	}
	if p.RecentRepliesForPrompt > 0 {
		c.RecentRepliesForPrompt = p.RecentRepliesForPrompt
	}
	if p.ReplyRate > 0 {
		c.ReplyRate = p.ReplyRate
	}
	if p.MaxBatchMsgs > 0 {
		c.MaxBatchMsgs = p.MaxBatchMsgs
	}
	if p.AtEffort != "" {
		c.AtEffort = p.AtEffort
	}
	if p.ProactiveEffort != "" {
		c.ProactiveEffort = p.ProactiveEffort
	}
	if len(p.Rules) > 0 {
		c.Rules = p.Rules
	}
	return c
}

// IncomingMessage is the minimal message view the observer needs from the
// platform (the host adapts its own message type into this one).
type IncomingMessage struct {
	SessionKey        string
	MessageID         string
	UserID            string
	UserName          string
	ChatName          string
	Content           string
	Mentioned         bool
	QuotedMessageID   string // platform message ID this message replies to (quote), if any
	MediaKind         string // "图片"/"语音"/"文件" when the message carries media (text may be empty)
	UserMessageTimeMs int64  // real platform message time (Unix ms); 0 = use arrival time
}

// Decision is the forwarding decision the host executes.
type Decision struct {
	Forward          bool
	Pending          bool   // deferred: decide after the in-flight turn completes
	Content          string // prompt/content to send; empty keeps the original
	Effort           string // reasoning effort override for this turn
	SuppressProgress bool   // hide thinking/tool progress for this turn
	SplitReplies     bool   // final response may carry several "---"-separated replies, each sent separately
	Direct           bool   // 路由判定：true = 高优回合（@/昵称，平台直通）；false = 普通回合（注入合成消息）
}

// ObservedMessage is one buffered group message (text plus identity metadata).
type ObservedMessage struct {
	At              time.Time
	MessageID       string
	UserID          string
	Sender          string
	Text            string
	QuotedMessageID string
	MediaKind       string
}

// replyEvent records when the bot replied (or was @-triggered) in a group,
// used for per-session frequency control and prompt annotations.
type replyEvent struct {
	At   time.Time
	Kind string // "proactive"（普通回合真实回复）| "at"（@ 高优回合）| "alias"（昵称高优回合）
}

type groupState struct {
	chatName        string
	buf             []ObservedMessage
	replies         []replyEvent
	pending         []pendingMsg // 普通回合候选（非 @/昵称）
	lastAct         time.Time
	lastMultiUserAt time.Time   // 多人规则最近一次触发时刻（冷却用，复用规则自身的窗口）
	lastForwardedAt time.Time   // newest message actually forwarded to the agent
	lastResetDay    string      // 上次做过每日上下文重置检查的北京时间日期（YYYYMMDD）
	noReplyTimes    []time.Time // 窗口内 NO_REPLY 时刻（按窗口剪枝）
	lastNoReplyTurn time.Time   // 降频期间最近一次实际转交批次的时间（2min 间隔用）
}

// pendingMsg is a normal-turn candidate deferred until the running turn completes.
type pendingMsg struct {
	msg   IncomingMessage
	entry ObservedMessage
}

const maxReplyEvents = 30
const maxPending = 500 // storage cap for the pending queue (most recent kept)

var beijingZone = time.FixedZone("CST", 8*3600)

// Observer is the decision layer (one per engine/project).
type Observer struct {
	cfg     Config
	ignore  []*regexp.Regexp
	busy    func(sessionKey string) bool
	forward func(sessionKey string, d Decision) error
	reset   func(sessionKey string, maxTokens int) bool // 宿主提供的“上下文超阈值则重置会话”回调

	mu        sync.Mutex
	groups    map[string]*groupState
	now       func() time.Time // 可注入的时钟（测试用）；nil = time.Now
	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
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
		ResetHour:              4,
		ResetContextThreshold:  0, // 默认不启用
		NoReplyThrottleMin:     0, // 默认不启用
		NoReplyThrottleGap:     120,
		AtEffort:               "medium",
		ProactiveEffort:        "low",
		SuppressGroupProgress:  true,
		MaxBatchMsgs:           100,
		Rules: []Rule{{
			Type:  "keyword",
			Regex: []string{"什么", "怎么", "吗", "你们", "？", "啥", "如何", "有没有"},
		}, {
			Type:      "multi_user",
			MinUsers:  999999, // 默认停用多人规则（保留代码）
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
// running, normal-turn decisions are deferred until it completes.
func (o *Observer) SetSessionBusy(fn func(sessionKey string) bool) {
	o.busy = fn
}

// SetForwarder registers the host callback that injects a decided message into
// the agent (used for deferred pending decisions).
func (o *Observer) SetForwarder(fn func(sessionKey string, d Decision) error) {
	o.forward = fn
}

// SetSessionResetter registers the host callback that resets a session to a
// fresh thread when its current context reaches maxTokens. Used by the daily
// oversized-context reset policy (reset_hour + reset_context_threshold).
func (o *Observer) SetSessionResetter(fn func(sessionKey string, maxTokens int) bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reset = fn
}

// Start 启动每日定时重置：睡到下一个 reset_hour（北京时间 04:00），到点对所有已
// 记录群会话批量执行一次“上下文超阈值则重置”检查（每个群每天一次），随后排下一次。
// 不轮询、不占用消息处理路径；错过当天窗口就等第二天，不设兜底。
// 未配置阈值或未接线宿主回调时为 no-op。
func (o *Observer) Start() {
	o.startOnce.Do(func() {
		o.mu.Lock()
		enabled := o.reset != nil && o.cfg.ResetContextThreshold > 0
		o.mu.Unlock()
		if !enabled {
			return
		}
		o.stopCh = make(chan struct{})
		go o.dailyResetLoop()
	})
}

// Close 停止每日批量重置的定时器（幂等）。
func (o *Observer) Close() {
	o.stopOnce.Do(func() {
		if o.stopCh != nil {
			close(o.stopCh)
		}
	})
}

func (o *Observer) dailyResetLoop() {
	for {
		next := o.nextResetTime(o.nowTime())
		d := time.Until(next)
		if d < 0 {
			d = 0
		}
		timer := time.NewTimer(d)
		select {
		case <-o.stopCh:
			timer.Stop()
			return
		case <-timer.C:
		}
		o.dailyBatchReset(o.nowTime())
	}
}

// nextResetTime 返回下一个 reset_hour（北京时间）的时刻：今天尚未到点则今天，否则明天。
func (o *Observer) nextResetTime(now time.Time) time.Time {
	bt := now.In(beijingZone)
	next := time.Date(bt.Year(), bt.Month(), bt.Day(), o.cfg.ResetHour, 0, 0, 0, beijingZone)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func (o *Observer) nowTime() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// dailyBatchReset 在 reset_hour 小时内批量检查所有已记录群会话。先把当天已检查的
// 标记好，再在锁外逐个调用宿主重置回调，避免持锁期间阻塞消息处理。
func (o *Observer) dailyBatchReset(now time.Time) {
	if o.cfg.ResetContextThreshold <= 0 || o.reset == nil {
		return
	}
	bt := now.In(beijingZone)
	if bt.Hour() != o.cfg.ResetHour {
		return
	}
	day := bt.Format("20060102")
	o.mu.Lock()
	var todo []string
	for key, st := range o.groups {
		if st.lastResetDay == day {
			continue
		}
		st.lastResetDay = day
		todo = append(todo, key)
	}
	o.mu.Unlock()
	for _, key := range todo {
		if o.reset(key, o.cfg.ResetContextThreshold) {
			slog.Info("observer: daily oversized-context reset requested",
				"session", key, "threshold", o.cfg.ResetContextThreshold)
		}
	}
}

// Decide is the group-message forwarding policy: it records the message,
// updates the forwarding point, and decides whether/how to forward it.
// @-bot messages and explicit nickname mentions are high-priority turns
// (高优回合) and always forward with at_effort; other messages are forwarded as
// normal turns (普通回合) when the activation rules hit.
func (o *Observer) Decide(msg IncomingMessage) Decision {
	if strings.TrimSpace(msg.Content) == "" && msg.MediaKind == "" {
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
	at := time.Now()
	if msg.UserMessageTimeMs > 0 {
		at = time.UnixMilli(msg.UserMessageTimeMs)
	}
	o.cfg = o.cfg.effective(at)
	entry := ObservedMessage{
		At:              at,
		MessageID:       msg.MessageID,
		UserID:          msg.UserID,
		Sender:          msg.UserName,
		Text:            truncate(msg.Content, 300),
		QuotedMessageID: msg.QuotedMessageID,
		MediaKind:       msg.MediaKind,
	}
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
		catchup := o.catchupBeforeLocked(st)
		st.pending = nil // 未转交的缓存随本次高优回合一并转交，避免后续重复打包
		prompt := o.buildTurnPrompt(st, entry, catchup, kind)
		o.mu.Unlock()
		slog.Info("observer: decision", "session", msg.SessionKey, "direct", true, "outcome", "forward")
		return Decision{Forward: true, Content: prompt, Effort: effortOr(o.cfg.AtEffort, "medium"), SuppressProgress: o.cfg.SuppressGroupProgress, SplitReplies: true, Direct: true}
	}

	// 普通回合候选：总是先入 pending 队列，再处理队列。Busy -> 等下一轮；
	// free -> "入队即出队"（立即处理）。未触发激活的条目继续留在队列，
	// 可随后续普通回合并一并评估。高优回合（@/昵称）立即决策，
	// 依赖 cc-connect 自身的队列机制。
	st.pending = append(st.pending, pendingMsg{msg: msg, entry: entry})
	if len(st.pending) > maxPending {
		st.pending = st.pending[len(st.pending)-maxPending:]
	}
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
	slog.Info("observer: decision", "session", msg.SessionKey, "busy", false, "outcome", "forward")
	return decisions[0]
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
	// 有真实回复：重置 NO_REPLY 降频计数与间隔状态
	st.noReplyTimes = nil
	st.lastNoReplyTurn = time.Time{}
	o.mu.Unlock()

	o.redecidePending(sessionKey)
}

// OnSilentTurn is called when a group turn completed without a real reply
// (NO_REPLY). It does not record a reply event (a silent turn is not a real
// reply for engage-window/frequency tracking) but still re-evaluates the
// pending queue so follow-up messages are not left to expire while the queue
// waits for the next message.
func (o *Observer) OnSilentTurn(sessionKey string) {
	o.mu.Lock()
	if st := o.groups[sessionKey]; st != nil {
		o.recordNoReplyLocked(st, time.Now())
	}
	o.mu.Unlock()
	o.redecidePending(sessionKey)
}

// recordNoReplyLocked 记录一次 NO_REPLY（按窗口剪枝后追加），并更新最近一次
// NO_REPLY 时刻。调用方必须持有 o.mu。
func (o *Observer) recordNoReplyLocked(st *groupState, at time.Time) {
	if o.cfg.NoReplyThrottleMin <= 0 {
		return
	}
	o.pruneNoReplyLocked(st, at)
	st.noReplyTimes = append(st.noReplyTimes, at)
}

// pruneNoReplyLocked 剪掉窗口外的 NO_REPLY 记录（复用 window_sec 消息缓存共享窗口）。
func (o *Observer) pruneNoReplyLocked(st *groupState, now time.Time) {
	w := time.Duration(o.cfg.WindowSec) * time.Second // 复用消息缓存共享窗口（window_sec）
	if w <= 0 {
		w = 10 * time.Minute
	}
	cutoff := now.Add(-w)
	keep := st.noReplyTimes[:0]
	for _, t := range st.noReplyTimes {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	st.noReplyTimes = keep
}

// noReplyThrottleAllowsLocked 判定本次普通回合转交是否放行：
// 窗口内 NO_REPLY 数未达阈值 -> 正常放行；
// 达阈值进入降频 -> 距上次实际转交批次不足 interval 则先不放行（pending 继续攒，
// 直到后续消息发现自己未被频控时整批转交），间隔已过则放行并记录本次批次时间。
func (o *Observer) noReplyThrottleAllowsLocked(st *groupState, now time.Time) bool {
	if o.cfg.NoReplyThrottleMin <= 0 {
		return true
	}
	o.pruneNoReplyLocked(st, now)
	if len(st.noReplyTimes) < o.cfg.NoReplyThrottleMin {
		return true
	}
	gap := time.Duration(o.cfg.NoReplyThrottleGap) * time.Second
	if gap <= 0 {
		gap = 2 * time.Minute
	}
	if st.lastNoReplyTurn.IsZero() || now.Sub(st.lastNoReplyTurn) >= gap {
		st.lastNoReplyTurn = now
		return true
	}
	slog.Info("observer: no-reply throttle hold")
	return false
}

// redecidePending re-evaluates deferred normal-turn candidates now that a turn
// has completed. If the session is busy again (another turn started), they
// keep waiting.
func (o *Observer) redecidePending(sessionKey string) {
	o.mu.Lock()
	cur := o.groups[sessionKey]
	if cur == nil || (o.busy != nil && o.busy(sessionKey)) {
		o.mu.Unlock()
		return
	}
	pending := cur.pending
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

// processPendingLocked evaluates the queued candidates as ONE batch: stale
// entries are dropped, and if the batch is worth a turn (the engage window is
// open or at least one candidate hits an activation rule, within
// cooldown/frequency limits), ALL non-stale candidates are merged into a single
// decision so the agent judges which of them to reply to in one pass. Returns
// at most one decision; candidates that did not justify a turn stay queued so
// they can ride along with a later batch (the rolling buffer keeps them as
// context in the meantime). The caller must hold o.mu.
func (o *Observer) processPendingLocked(st *groupState) (decisions []Decision, requeue []pendingMsg) {
	o.cfg = o.cfg.effective(time.Now())
	live := make([]pendingMsg, 0, len(st.pending))
	for _, p := range st.pending {
		if !o.isStale(p.entry.At) {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		return nil, nil
	}
	batch := live
	if n := o.cfg.MaxBatchMsgs; n > 0 && len(batch) > n {
		batch = batch[len(batch)-n:]
	}
	if !o.batchActivateLocked(st, batch) {
		return nil, live // not worth a turn now; keep queued for a later batch
	}
	kind := ""
	if o.engageActive(st) {
		kind = "engage"
	}
	prompt := o.buildBatchPrompt(st, batch, kind)
	st.lastForwardedAt = batch[len(batch)-1].entry.At
	return []Decision{{
		Forward:          true,
		Content:          prompt,
		Effort:           effortOr(o.cfg.ProactiveEffort, "low"),
		SuppressProgress: o.cfg.SuppressGroupProgress,
		SplitReplies:     true,
	}}, nil
}

// batchActivateLocked decides whether the pending batch deserves one AI turn.
// Cooldown / min-interval / max-per-hour guards apply to the batch as a whole
// (one batch = one potential normal turn); then either the engage window is
// open or at least one candidate matches an activation rule. The caller must
// hold o.mu.
func (o *Observer) batchActivateLocked(st *groupState, batch []pendingMsg) bool {
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
	if o.engageActive(st) {
		if !o.noReplyThrottleAllowsLocked(st, now) {
			return false // 降频中且未到间隔：本次不转交，pending 继续攒
		}
		st.lastAct = now
		return true
	}
	for _, p := range batch {
		if matched := o.rulesMatch(st, p.msg.Content, now); matched != "" {
			if matched == "multi_user" {
				st.lastMultiUserAt = now
			}
			if !o.noReplyThrottleAllowsLocked(st, now) {
				return false // 降频中且未到间隔：本次不转交，pending 继续攒
			}
			st.lastAct = now
			return true
		}
	}
	return false
}

// isStale reports whether a buffered/pending message is too old to reply to,
// using the unified time window (window_sec).
func (o *Observer) isStale(at time.Time) bool {
	w := time.Duration(o.cfg.WindowSec) * time.Second
	return w > 0 && time.Since(at) > w
}

// buildTurnPrompt assembles the prompt for a high-priority turn (高优回合,
// @-directed or nickname-called): persona, the latest message (the actual
// trigger), the supplementary recent dialogue (others' messages plus the
// bot's own recent replies) and the requirements for this turn. kind is "at"
// (@ 高优，积极回复) or "alias" (昵称高优；判断是否真的直接跟机器人说话).
// The "engage"/"" branches are kept for completeness but are not reachable
// from Decide today.
func (o *Observer) buildTurnPrompt(st *groupState, trigger ObservedMessage, catchup []ObservedMessage, kind string) string {
	var sb strings.Builder
	o.writePersona(&sb)

	// 要求段放在消息之前：同场景回合的身份+要求前缀完全一致，可提升前缀缓存命中率。
	sb.WriteString("\n【本次要求】\n")
	switch kind {
	case "at":
		sb.WriteString("- 这条消息是被 @ 的直接对话：应当积极回应、一般都要回复，除非实在无话可说，否则不要输出 NO_REPLY；\n")
	case "alias":
		fmt.Fprintf(&sb, "- 本条提到了你的昵称：先结合上下文判断是否真的是直接跟你说话；是 → 积极回应；否（只是提到/转述等）→ 按“最值得回复”自我评价，只参与最值得回复的前 %d%% 的话题（不硬凑、不勉强）；\n", replyRate(o.cfg))
		sb.WriteString("- 判断为直接对话时不要输出 NO_REPLY；判断为并非直接对话时，不值得参与就输出 NO_REPLY；\n")
	case "engage":
		fmt.Fprintf(&sb, "- 依据消息时间与文本判断是否在与自己对话；是 → 积极回应；否 → 按“最值得回复”自我评价，只参与最值得回复的前 %d%% 的话题（不硬凑、不勉强）；\n", replyRate(o.cfg))
		sb.WriteString("- 判断为在跟你对话（追问/接话/点名）时不要输出 NO_REPLY；判断并非对你所说时，不值得参与就输出 NO_REPLY；\n")
	default:
		fmt.Fprintf(&sb, "- 这条消息没有被 @ 也没有叫你：按“最值得回复”自我评价，只参与最值得回复的前 %d%% 的话题（不硬凑、不勉强）；\n", replyRate(o.cfg))
		sb.WriteString("- 不值得参与时只输出 NO_REPLY，不要发任何其他内容；\n")
	}
	o.writeCommonReplyRules(&sb)

	sb.WriteString("\n【最新消息】\n\n")
	sb.WriteString(describeTrigger(trigger))

	o.writeSupplementContext(&sb, st, catchup)
	return sb.String()
}

// buildBatchPrompt assembles the prompt for a normal turn (普通回合):
// persona, the requirements, a clearly marked candidate section (排队候选，
// 需要判断是否回复) and a separate supplementary recent-dialogue block
// (背景他人对话 + 你的近期回复). The two are deliberately unequal in status:
// candidates are the messages to judge, the others are background only.
// kind is "engage" (inside the engage window; judge first whether each message
// is directed at the bot) or "" (ambient batch; reply only to the top-worth
// candidates).
func (o *Observer) buildBatchPrompt(st *groupState, batch []pendingMsg, kind string) string {
	var sb strings.Builder
	o.writePersona(&sb)

	sb.WriteString("\n【本次要求】\n")
	sb.WriteString("- 需要你判断这些消息是否值得回复，选择性回复；\n")
	sb.WriteString("- 媒体消息（图片/语音/文件）不需要判断是否回复，仅作上下文；\n")
	if kind == "engage" {
		fmt.Fprintf(&sb, "- 先依据消息时间与文本判断是否在与自己对话；是 → 积极回应；否则按“最值得回复”自我评价，只参与最值得回复的前 %d%% 的话题（不硬凑、不勉强）；\n", replyRate(o.cfg))
	} else {
		fmt.Fprintf(&sb, "- 按“最值得回复”自我评价，只参与最值得回复的前 %d%% 的话题（不硬凑、不勉强）；\n", replyRate(o.cfg))
	}
	o.writeCommonReplyRules(&sb)
	sb.WriteString("- 没有值得回复的就只输出 NO_REPLY，不要发任何其他内容。\n")

	sb.WriteString("\n【待处理消息】\n\n")
	fmt.Fprintf(&sb, "以下是此前排队、尚未处理的 %d 条群消息（按时间顺序）：\n", len(batch))
	for i, p := range batch {
		sb.WriteString(describeObserved(i+1, p.entry))
	}

	o.writeSupplementContext(&sb, st, o.batchContextLocked(st, batch))
	return sb.String()
}

// writeSupplementContext writes the supplementary recent-conversation block:
// other people's recent dialogue and the bot's own recent replies as parallel
// sub-lists, so the model can tell that all of it is background context.
// Either half appears only when there is content.
func (o *Observer) writeSupplementContext(sb *strings.Builder, st *groupState, others []ObservedMessage) {
	var ob strings.Builder
	for i, m := range others {
		ob.WriteString(describeObserved(i+1, m))
	}
	replies := o.recentRepliesText(st)
	if ob.Len() == 0 && replies == "（无记录）\n" {
		return
	}
	sb.WriteString("\n【补充近期对话】\n")
	if ob.Len() > 0 {
		sb.WriteString("\n他人对话：\n")
		sb.WriteString(ob.String())
	}
	if replies != "（无记录）\n" {
		sb.WriteString("\n你的近期回复（注意频控，避免高频刷屏）：\n")
		sb.WriteString(replies)
	}
}

// writeCommonReplyRules writes the reply rules shared by every turn type
// (direct @/nickname turns and proactive batch turns). Keeping them in one
// place prevents rule drift between prompt variants: previously the
// "思考过程不要写进回复" guard only existed in the direct-turn prompt, so
// proactive batch turns could leak their decision rationale before "---".
func (o *Observer) writeCommonReplyRules(sb *strings.Builder) {
	sb.WriteString(`- 口语化，不要列条陈词、不要像客服；
- 不涉及分析时简短回复，常规分析类回复应控制在300字内（除非在该限制下不足以阐述完整重点/对方再次追问/对方要求更详细展开的情况可破例）；
- 思考过程不要写进回复，只输出最终结论；
- 回复不同的人或不同主体时，用单独一行 --- 分隔，每条分别作为独立消息发出；
- 如确需理解引用/图片/文件消息的原文，可用 OneBot v11 get_msg 协议按消息 id 查询（方法与边界见 RULES_QQ_REPLY.md；非必要不要查）；
- 遵守 RULES_SAFE 群聊安全规则，不暴露任何本机路径、配置、IP 等隐私信息。
`)
}

// batchContextLocked returns recent buffer messages around the batch as
// background context: observed messages after the last forwarding point and
// within the shared window, excluding the candidates already listed in the
// batch (they are the action items, not background). Total text is capped by
// characters, keeping the most recent entries. The caller must hold o.mu.
func (o *Observer) batchContextLocked(st *groupState, batch []pendingMsg) []ObservedMessage {
	inBatch := make(map[ObservedMessage]bool, len(batch))
	for _, p := range batch {
		inBatch[p.entry] = true
	}
	window := time.Duration(o.cfg.WindowSec) * time.Second
	var cand []ObservedMessage
	for _, m := range st.buf {
		if inBatch[m] {
			continue
		}
		if !m.At.After(st.lastForwardedAt) {
			continue
		}
		if window > 0 && time.Since(m.At) > window {
			continue
		}
		cand = append(cand, m)
	}
	const maxContextChars = 2000
	total := 0
	start := 0
	for i := len(cand) - 1; i >= 0; i-- {
		total += len([]rune(cand[i].Text))
		if total > maxContextChars {
			start = i + 1
			break
		}
	}
	return cand[start:]
}

// catchupBeforeLocked returns the queued non-direct candidates (still within
// the shared window) as background context for a direct @/nickname turn. Total
// text is capped by characters, keeping the most recent entries; the caller
// must hold o.mu and consume st.pending afterwards so they are not re-sent.
func (o *Observer) catchupBeforeLocked(st *groupState) []ObservedMessage {
	live := make([]ObservedMessage, 0, len(st.pending))
	for _, p := range st.pending {
		if !o.isStale(p.entry.At) {
			live = append(live, p.entry)
		}
	}
	const maxCatchupChars = 2000
	total := 0
	start := 0
	for i := len(live) - 1; i >= 0; i-- {
		total += len([]rune(live[i].Text))
		if total > maxCatchupChars {
			start = i + 1
			break
		}
	}
	return live[start:]
}

func qqSuffix(uid string) string {
	if uid == "" {
		return ""
	}
	return "（QQ " + uid + "）"
}

// describeObserved renders one observed message with its own message id,
// quoted id and media marker, e.g.:
//  01. [mid=<消息id>] 16:03 群昵称（QQ <号码>）：text〔引用 id <被引用消息id>〕〔图片〕
func describeObserved(idx int, m ObservedMessage) string {
	mid := ""
	if m.MessageID != "" {
		mid = "[mid=" + m.MessageID + "] "
	}
	return fmt.Sprintf("%02d. %s%s %s%s：%s%s%s\n",
		idx, mid, m.At.Format("15:04"), m.Sender, qqSuffix(m.UserID),
		m.Text, quotedSuffix(m.QuotedMessageID), mediaSuffix(m.MediaKind))
}

// describeTrigger renders the triggering message of a high-priority turn
// (高优回合) with the same id/quote/media markers as batch messages.
func describeTrigger(m ObservedMessage) string {
	mid := ""
	if m.MessageID != "" {
		mid = "[mid=" + m.MessageID + "] "
	}
	return fmt.Sprintf("%s%s %s：%s%s%s\n",
		mid, m.At.Format("15:04"), m.Sender, m.Text,
		quotedSuffix(m.QuotedMessageID), mediaSuffix(m.MediaKind))
}

func quotedSuffix(qid string) string {
	if qid == "" {
		return ""
	}
	return "〔引用 id " + qid + "〕"
}

func mediaSuffix(kind string) string {
	switch kind {
	case "图片":
		return "〔图片〕"
	case "语音":
		return "〔语音〕"
	case "文件":
		return "〔文件〕"
	}
	return ""
}

// writePersona writes the persona block shared by all turn prompts.
func (o *Observer) writePersona(sb *strings.Builder) {
	persona := o.cfg.PersonaName
	if strings.TrimSpace(persona) == "" {
		persona = "本群机器人"
	}
	fmt.Fprintf(sb, `【你的身份】

你是这个群里“%s”，是大家眼里的熟朋友：爱聊天、爱接梗，也喜欢帮大家解答各种问题（生活、游戏、
炒股都可以），乐于分享自己的看法，有自己的态度，不谄媚、不机械。
`, persona)
	if alias := strings.TrimSpace(o.cfg.PersonaAlias); alias != "" {
		fmt.Fprintf(sb, "- 大家也常叫你“%s”，别人明确这样叫通常就是在直接跟你说话；\n", alias)
	}
	sb.WriteString("- 模型本体：你是 DeepSeek 系模型，不是 GPT/OpenAI 系；被问具体型号时以 codex 配置中的真实模型版本为准，按 DeepSeek 系如实回答。\n")
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

// rulesMatch returns the type of the first configured rule that matches the
// message/group state ("" if none). v1 implements "keyword"; "multi_user"
// counts distinct participants in the recent window (the bot itself never
// enters the buffer, so it is never counted) and is throttled by a cooldown
// that reuses the same window: once it fires, it won't fire again within the
// window regardless of whether the AI replied; other types are reserved.
func (o *Observer) rulesMatch(st *groupState, content string, now time.Time) string {
	for _, r := range o.cfg.Rules {
		switch strings.ToLower(strings.TrimSpace(r.Type)) {
		case "", "keyword":
			for _, p := range r.Regex {
				if re, err := regexp.Compile(p); err == nil && re.MatchString(content) {
					return "keyword"
				}
			}
		case "multi_user":
			window := time.Duration(r.WindowSec) * time.Second
			if window <= 0 {
				window = 5 * time.Minute
			}
			// 冷却：与多人规则窗口复用同一个变量，窗口内不重复触发
			if !st.lastMultiUserAt.IsZero() && now.Sub(st.lastMultiUserAt) < window {
				continue
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
				return "multi_user"
			}
		default:
			// reserved: burst / multi_user / random / ...
		}
	}
	return ""
}

func lastProactive(st *groupState) time.Time {
	for i := len(st.replies) - 1; i >= 0; i-- {
		if st.replies[i].Kind == "proactive" {
			return st.replies[i].At
		}
	}
	return time.Time{}
}

// lastReply returns the most recent reply event of any kind (proactive, @, or
// alias). It backs the engage window so a follow-up after ANY reply is treated
// as a continuation of the conversation.
func lastReply(st *groupState) time.Time {
	for i := len(st.replies) - 1; i >= 0; i-- {
		return st.replies[i].At
	}
	return time.Time{}
}

// engageActive reports whether the engage window is currently open, i.e. a
// real reply of any kind happened within the engage window (engage_window_sec,
// falling back to window_sec).
func (o *Observer) engageActive(st *groupState) bool {
	last := lastReply(st)
	w := o.cfg.EngageWindowSec
	if w <= 0 {
		w = o.cfg.WindowSec
	}
	if last.IsZero() || w <= 0 {
		return false
	}
	return time.Since(last) < time.Duration(w)*time.Second
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
