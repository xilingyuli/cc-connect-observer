# cc-connect-observer

cc-connect 的「群聊视奸」被动插话插件（独立仓库、独立配置、独立测试）。

## 与主体的关系

- cc-connect 主体只做两件事：总开关（config.toml 顶层 `observer`，值为本插件配置文件的
  绝对/相对路径）+ 平台钩子（群消息路由进本插件）。
- 本插件通过 go.mod 的 `replace` 指向本地 cc-connect 主体源码，依赖其 `core` 类型
  （Engine / Message / CronJob）。
- 主体测试与本插件测试完全隔离（不同 module，互不干扰）。

## 工作方式

- 每群维护内存滚动观察缓冲（可丢，重启即清空）。
- 群消息（含 @ 消息）写入缓冲；命中激活规则且通过频控时，起一个独立观察会话
  （`new_per_run`），把最近 N 条记录给模型，由模型自主决定插话或 `NO_REPLY`。
- 视奸回合静默执行（无 ⏰/💭 进度消息）；频控按会话（群）独立：
  冷却、最小间隔、每小时上限，提示词中标注近 10 次发言/触发记录。

## 配置

部署为 `<cc-connect 配置同目录或任意路径>/observer.toml`，主体 config.toml 里：

```toml
observer = "/abs/path/to/observer.toml"
```

规则列表支持后续扩展（v1 已实现 `keyword`，预留 burst / multi_user / random）。
