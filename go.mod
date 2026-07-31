module github.com/xilingyuli/cc-connect-observer

go 1.25.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/chenhg5/cc-connect v0.0.0
)

require (
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
)

// 本地开发：指向 cc-connect 主体源码（含本插件依赖的 Message/CronJob 扩展字段）。
replace github.com/chenhg5/cc-connect => ../cc-connect-source
