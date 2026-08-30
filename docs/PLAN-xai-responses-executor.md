# 移植计划：xai-oauth 插件内置 Responses executor（暂缓实施）

> 状态：**已分析，未动工**（记录于 2026-08-31）。触发条件：遇到 `/chat/completions` 路径
> 引发的问题，或出现 Responses-only 功能需求时，按本计划在分析基础上实施。
>
> 参照仓库：`~/src/CLIProxyAPI`（宿主，内置 xai executor 所在地）。

## 背景：风险 3（README "Known limitations"）

当前插件推理走宿主通用 OpenAI-compat 路径（`/chat/completions`），而内置 xai 走
Grok CLI 的 Responses API。chat-proxy 两个端点都收，但 Responses-only 能力缺失：
`/responses/compact`、reasoning `encrypted_content` 回放、x_search 工具注入/过滤、
免费额度冷却处理、`/v1/responses` websocket 透传。

宿主能力边界（好消息，不需要改核心）：
- 插件 ABI 原生支持 `Executor` capability：`sdk/pluginabi/types.go:42-46`
  （`executor.identifier/execute/execute_stream/count_tokens/http_request`）。
- 宿主适配器 `internal/pluginhost/adapters_executors.go` 会在 provider **没有**原生
  executor 时挂接插件 executor（`adapters_executors.go:174`）——`xai-oauth` 恰好满足，
  内置 `xai` 不受影响。
- `executor.execute` 入参自带 host 代理感知 `HTTPClient`（`sdk/pluginapi/types.go:597-600`）、
  `Alt`（识别 compact）、`StorageJSON`/`AuthAttributes`；声明
  `ExecutorInputFormats/OutputFormats` 后，客户端协议 ⇄ Responses 形状的翻译由宿主
  `sdk/translator` 完成，插件只做 xAI 特有补丁。
- 现有插件本体：非测试代码 ~1,900 行；全套做完体积翻 ~2.5 倍。

## 内置实现规模（参照基线）

核心 HTTP Responses 路径 ~3.2k 行非测试代码（6 个文件共 ~4.9k，扣 websocket 1,699
与 media ~0.3k）。其中 **tool/schema 处理占 40–45%**（~830 行，
`xai_executor_request.go:639-1373`），全是上游怪癖的编码。内置 5,510 行测试在
`internal` 包，一行搬不过来，全部重写。

关键上游行为备忘：
- **"非流式"也是伪 SSE**：`/responses` 即使 `stream=false` 也回 `data:` 行，需收集
  `response.output_item.done` 并 patch 进 `response.completed`
  （`xai_executor_execute.go:84-121`）。
- **compact 必须走 `api.x.ai`**：chat-proxy 对 `/responses/compact` 返回 404，且 404
  会冷却整个 auth 池（`xai_executor_execute.go:140-141`、`xai_executor_request.go:248-254`）。
- **错误映射语义**：403 `bad-credentials` → 401（触发 OAuth 刷新重试）；429
  `free-usage-exhausted` → 24h retryAfter 冷却（`xai_executor_response.go:869-895`）。
- **codex_app.automation_update 真 schema 会让 chat-proxy 挂起不发 SSE**，内置替换为宽松
  占位 `xaiSafeFunctionParameters`（`xai_executor.go:30-36`）。
- x_search 注入是内置 config 开关（`cfg.XAI.InjectXSearch`）；过滤对象是 `xs_call*`
  子工具轨迹（`x_user_search`/`x_semantic_search`/`x_keyword_search`/`x_thread_fetch`），
  需重编 `output_index`（`xai_executor_response.go:23-281`）。
- websocket 透传：插件 ABI 无 websocket 客户端，且与 codex executor 共享 ~800 行
  session 层 → **插件架构下不可行**，作为硬边界写进 README，不排期。

## 任务表（优先级排序）

| 优先级 | 工作项 | LOC (prod/test) | 工作量 | 依赖 | 做的理由 | 不做的理由 |
|---|---|---|---|---|---|---|
| P0 | ① Executor 骨架：register 声明 + `executor.*` RPC 分发 + `host.http.do_stream` 流泵 + HTTPClient 桥接 | ~300/~200 | 1–1.5d | — | 不声明就永远锁死在 OpenAI-compat 路径；骨架先跑通验证 RPC/流泵假设 | 若决定不碰 Responses，300 行全白写 |
| P0 | ② 主路径 + 伪 SSE 解析 | ~450/~400 | 1.5–2d | ① | `/chat/completions` 拿不到 Responses 原生语义（stop_reason、reasoning、output_item）；一切地基 | 占 MVP 一半工时；客户端全是 chat-completions 消费方且不需思考展示则收益趋零 |
| P0 | ③ 错误映射 | ~80/~120 | 0.5d | ② | 403→401 才有 token 过期自动恢复，否则凭证永久报废；429 冷却不做会打爆上游。**性价比全表第一** | 无——正确性补丁，迟早会撞 |
| P0 | ④ 基础请求整形 | ~250/~250 | 1d | ① | chat-proxy 硬性拒绝/忽略某些翻译后字段（`stop`、越界 effort、`previous_response_id`） | 清单需先用 3–5 个真实 body 实测确认 |
| | **MVP（①–④）** | **~1.1k/~1.0k** | **~4–5d** | | 端点对齐 + 正确性兜底 | 从未实际出故障时可只保 ③ |
| P1 | ⑤ 工具/schema 归一化：namespace 扁平化、`custom`→`function`、`tool_choice` 级联、`automation_update` 占位 | ~750/~700 | 2.5–3d | ②④ | Codex Desktop 挂起、namespace/custom 工具 400 的根源全在这 830 行 | 最大单项且全是只能实测的怪癖；没有 Codex/Desktop 客户端就是死重 |
| P1 | ⑥ x_search 注入 + 轨迹过滤 | ~280/~250 | 1d | ⑤ | 免费 OAuth 凭证获得联网搜索；防内部轨迹泄漏 | 内置默认关（config 开关）；不做只是少特性 |
| P1 | ⑦ reasoning summary 事件归一化（非标 `response.reasoning_text.*` → 标准，1 拆 2） | ~150/~150 | 0.5–1d | ② | 150 行修好 Claude 系思考展示，P1 性价比最高 | 客户端不消费 reasoning 就无感；便宜，省它意义不大 |
| P2 | ⑧ compact 路由（强制 `api.x.ai`） | ~70/~80 | 0.5d | ①③ | 70 行；路由错了 404 会冷却整池，比没做更糟 | 客户端不发 compact 就是死代码 |
| P2 | ⑨ reasoning 回放：自建缓存（model+session, 过期）+ 签名校验重写 | ~350/~300 | 1.5–2d | ②③ | Codex 多轮推理连续性（`internal/signature` 不可 import，须重写） | 状态复杂、跨 reload 丢失；收益依赖真实 Codex 用户 |
| P2 | ⑩ 用量上报重建（usage → envelope metadata/host 回调） | ~120/~100 | 1d | ② | 否则管理面板统计成为用量盲区 | 纯可观测性，可一直拖 |
| P3 | ⑪ 漂移防护（`grokClientVersion` + 行为开关配置化）+ README 限制更新 | ~60 + 文档 ~80 | 0.5d | 全部 | 移植后漂移代价从"bump 常量"变"同步一块逻辑"，不做 ⑪ 等于埋雷 | 不移植则无需做 |
| — | ⑫ websocket 透传 | **不可行** | — | — | 只能走宿主 PR 扩 ABI，超出插件范畴 | |
| — | ⑬ media (images/videos) | 不在范围 | — | — | 需 API key 语义，维持内置 | |
| | **合计** | **~2.9k prod + ~2.6k test ≈ 5.5k 行** | **≈ 2 周** | | | |

依赖链：**①→②→{③④} 为 MVP 串行主链；⑤ 依赖主链且是 ⑥ 前置；⑦⑧⑨⑩ 各只依赖
主链，可并行；⑪ 收尾。**

## 决策线（实施前重读）

1. **客户端构成定范围**：只有 Claude 系 → 做到 **MVP+⑦** 即停（~2.2k 行/~5 天），
   ⑤⑥⑨ 是死重；有 Codex/agent 重度用户 → ⑤⑨ 必做，全套两周起。
2. **当前是否真的疼**：风险 3 至今零故障。没人报 tool call 丢失/挂起 → 只上 MVP
   （③ 无论如何建议做）。
3. **维护承诺**：做完全套，项目性质从"OAuth 登录插件"变为"跟着 chat-proxy 漂移的
   迷你 xAI executor"。⑪ 只能缓解不能消除。不想背 → 停在 MVP + README 写清限制。

## 实施提示（届时看）

- executor 部分单独开文件/子包（`executor*.go`、`reasoning_replay.go`、`usage.go`
  约 8 个新文件），别让 `main` 包膨胀；`go.mod` 需 +gjson/sjson。
- 内置依赖的 `internal/runtime/executor/helps`（30 个符号）不可 import：usage reporter、
  错误摘要、代理 HTTP client 用 host RPC 或自建替代；`internal/cache`、`internal/signature`
  同理需重写。`sdk/translator`、`sdk/cliproxy/executor`（类型）可直接 import。
- ⑤ 之前先用真实流量抓 3–5 个请求/响应 body 固化成测试夹具，移植-验证缺一不可。
- 参考宿主示例：`examples/plugin/executor/`、`examples/plugin/protocol-format/`
  （轻量替代路线：若某天只要修补个别字段差异，`response.intercept_stream_chunk` /
  `request.translate` 钩子几百行可解决，见 `sdk/pluginabi/types.go:48-58`）。
- 版本漂移：`grokClientVersion`（本仓库 `oauth.go:29`）与宿主
  `internal/runtime/executor/xai_executor.go` 的同名常量当前一致（0.2.120）。
