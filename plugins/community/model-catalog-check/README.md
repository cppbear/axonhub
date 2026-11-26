# Model catalog check · 模型目录检查

检查启用及停用渠道显式配置的模型、额外选定服务商的新模型，以及全局模型已跟随的参考价。每个模型只计一次。固定参考价由管理员维护，检查只读。

在包含任务 GraphQL 和角标支持（#3095）的 AxonHub 中安装并授予 `read_channels`。启用后默认每天 08:30 UTC 检查。Owner 可在插件详情修改 cron 和时区、暂停或立即运行。在插件设置填入「模型 → 导入与同步」中显示的服务商 ID，可选按末段模型名匹配渠道模型。留空服务商列表仍会检查已跟随的参考价。

所有已登录成员都能看到顶部角标、上次成功检查的时间、范围（包括服务商）和两项计数。读取上游失败时保留上次完整结果。站点渠道编辑者可从「复核最新差异」按上次范围预览、勾选并应用；详细模型、渠道和价格只在原生预览中显示。打开复核需要支持 `catalogReview` 的模型页，旧版模型页只打开模型列表。

## Installation

Grant `read_channels` and enable the plugin. The default schedule is 08:30 UTC daily. Owners can change the time zone and cron, pause the schedule, or run it immediately. Choose provider IDs from the native **Models → Import and sync** dialog. Configured channel models are included by default. Followed reference prices are checked across providers even when the additional provider list is empty.

All signed-in members can see the last check time, scope (including provider IDs), and counts. Site channel editors can review the current differences in that scope and choose changes to apply. Checks never change model or channel prices.

```sh
go run -C cmd/pack-plugin . ../../plugins/community/model-catalog-check
```
