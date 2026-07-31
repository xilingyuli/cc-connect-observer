# cc-connect-observer

cc-connect 的「群聊影子人格」被动插话插件（独立仓库、独立配置、独立测试）。

## 架构（决策层）

cc-connect 托管所有 codex 会话（同一群共享同一线程，@ 与影子回合都走它，天然连续）。
本插件只做决策：

- 每条群消息是否转交、转交的提示词内容；
- 转交时用的推理强度（@ 回合 `at_effort=high`，影子回合 `shadow_effort=low`）；
- 是否暴露思考过程（影子回合静默）。

## 工作方式

- 每群维护内存滚动观察缓冲（可丢，重启即清空），记录所有群消息文本；
- @ 消息：总是转交主会话（high 强度、正常显示思考）；
- 非 @ 消息：命中激活规则（keyword，后续可扩展）且通过频控时，作为影子回合转交
  （low 强度、隐藏思考）；真实回复经主体的 `message.sent` 钩子回灌，用于频控与
  engage 窗口（NO_REPLY 不计入）；
- **补转交**：影子触发时，自动补上“上次真实转交点”到本次触发之间未转交的最近文本
  消息，带条数 / 时间窗口 / 总长度上限过滤，图片视频等只读文本的天然不补。

## 配置

部署为 `<cc-connect 配置同目录或任意路径>/observer.toml`，主体 config.toml 里：

```toml
observer = "/abs/path/to/observer.toml"
```

关键项：`at_effort` / `shadow_effort` / `suppress_shadow_progress`、
`window_sec`（补转交过滤 / pending 超时 / 持续投递共用）、
`catchup_max_msgs` / `catchup_max_chars`、`reply_rate`、`[[rules]]`。
