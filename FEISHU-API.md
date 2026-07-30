# lark-bridge 飞书开放平台接口文档

**最后更新**：2026-07-27
**代码基线**：`main`（含 status-monitor 改动）
**范围**：lark-bridge 对飞书开放平台的**全部**接口调用——REST 发消息/更新消息、tenant_access_token 鉴权、WebSocket 长连接（事件接收/卡片回调/心跳/ACK/重连）。
**实现说明**：本项目**未使用任何第三方飞书 SDK**（已移除 `oapi-sdk-go`、`gorilla/websocket`、`gogo/protobuf`）。全部接口在 `internal/lark/`（客户端核心）+ `internal/feishu/`（业务封装）自实现：手写 RFC 6455 WebSocket 客户端 + 手写 protobuf 帧编解码 + REST 收发。

> 所有断言带 `file:line`，精确到源行。仅 `internal/lark/` 与 `internal/feishu/` 直接调飞书 API；`backendrpc`/`miniagent` 中的 HTTP 调用目标是项目自有后端，不属于飞书接口。

---

## 0. 调用面总览

| # | 分类 | 方法 | 路径 / 帧类型 | 实现位置 |
|---|------|------|---------------|----------|
| 1 | 鉴权 | POST | `/open-apis/auth/v3/tenant_access_token/internal` | `internal/lark/auth.go:113` |
| 2 | 消息发送（新建） | POST | `/open-apis/im/v1/messages?receive_id_type=chat_id` | `internal/lark/rest.go:66` |
| 3 | 消息发送（回复） | POST | `/open-apis/im/v1/messages/{message_id}/reply` | `internal/lark/rest.go:57` |
| 4 | 消息更新 | PATCH | `/open-apis/im/v1/messages/{message_id}` | `internal/lark/rest.go:83` |
| 5 | WebSocket 引导 | POST | `/callback/ws/endpoint` | `internal/lark/ws/wsclient.go:206` |
| 6 | WebSocket 拨号 | GET（Upgrade） | 服务器签名返回的 wss URL | `internal/lark/websocket/dial.go:42` |
| 7 | 收事件 `im.message.receive_v1` | WS Binary 帧 | — | `internal/lark/ws/dispatcher.go:168` |
| 8 | 卡片回调 `card.action.trigger` | WS Binary 帧 | — | `internal/lark/ws/dispatcher.go:174` |
| 9 | Ping 心跳 | WS Binary 帧 | — | `internal/lark/ws/session.go:223` |
| 10 | ACK 事件回执 | WS Binary 帧 | — | `internal/lark/ws/session.go:194` |

**域名**（`resolveBaseURL`，`internal/lark/client.go:37-48`）：空/`"feishu"` → `https://open.feishu.cn`；`"larksuite"`/`"lark"` → `https://open.larksuite.com`；带 scheme 原样；裸 host → `https://<host>`。三个 baseURL（REST/Token/WS bootstrap）在 `NewClient` 用同一个值构造（`internal/lark/client.go:87-91`）。bootstrap 返回的 wss URL 由飞书签名，域名随调用域。

---

## 1. 鉴权：获取 tenant_access_token

- **分类**：鉴权
- **HTTP 方法**：`POST`
- **完整 URL**：`{baseURL}/open-apis/auth/v3/tenant_access_token/internal`
- **请求结构**（`internal/lark/auth.go:108-111`）：

  | 字段 | Go 类型 | 含义 |
  |------|---------|------|
  | `app_id` | string | 自建应用 AppID |
  | `app_secret` | string | 自建应用 AppSecret |

  请求头：`Content-Type: application/json; charset=utf-8`（`:117`）。**无 Authorization 头**（本接口即换取令牌）。

- **响应结构**（`tokenResponse`，`internal/lark/auth.go:48-53`）：

  | 字段 | Go 类型 | 含义 |
  |------|---------|------|
  | `code` | int | 业务码，0 = 成功 |
  | `msg` | string | 错误描述 |
  | `tenant_access_token` | string | 租户令牌，后续 REST 调用 Bearer |
  | `expire` | int | 有效期（秒），生产通常 7200 |

- **调用点**：HTTP 发送 `internal/lark/auth.go:118`；`tokenManager.fetch` `:107-140`；缓存入口 `tokenManager.Token` `:59-104`；并发单飞防重复刷新 `:67-81`。
- **上游业务用途**：所有 REST 调用经 `restClient.doJSON` → `r.tokens.Token(ctx)`（`internal/lark/rest.go:111`）取令牌；提前 `tokenRefreshLead = 5min` 主动刷新（`internal/lark/auth.go:17,61`）。
- **鉴权方式**：Body 内 `app_secret` 即凭据。
- **错误码处理**：
  - HTTP 非 200 → `lark: token http %d: <body>`（`:129-131`）。
  - `code != 0` 或 token 空 → `*APIError{Code,Msg}`（`:136-138`），`Error()` 形如 `code:<N> msg:<M>`（`:150-152`）——这是下游 `strings.Contains(s,"code:...")` 匹配的基础。
  - 读 body 上限 `maxAuthBodyBytes = 1MiB`（`:22,125`）。
  - **不重试**：失败直返，由调用方决定。

---

## 2. 消息发送（新建 + 回复，同一入口两种路径）

### 2.1 公共请求构造

- **分类**：消息发送
- **HTTP 方法**：`POST`（两路径都是）
- **完整 URL**：
  - **新建**：`{baseURL}/open-apis/im/v1/messages?receive_id_type=chat_id`（`internal/lark/rest.go:66-67`）
  - **回复**：`{baseURL}/open-apis/im/v1/messages/{ReplyMessageID}/reply`（无 query；`ReplyMessageID` 经 `url.PathEscape`；`:57`）
- **请求结构**（`internal/lark/rest.go:54-65`）：

  | 字段 | Go 类型 | 含义 | 关键示例值 |
  |------|---------|------|-----------|
  | `msg_type` | string | 消息类型 | `"text"` / `"interactive"` |
  | `content` | string | 内层 JSON 字符串 | text: `{"text":"..."}`；card: 卡片 JSON 原样 |
  | `receive_id` | string | 仅新建时设置 | `SendInput.ChatID`（`chat_id`） |

  编码逻辑（`encodeSendContent`，`:90-105`）：`Text` 非空 → `msg_type=text`；`Card` 非空 → `msg_type=interactive`；两者互斥；都空报错。

  入口 `SendInput`（`internal/lark/types.go:17-22`）：

  | 字段 | 含义 |
  |------|------|
  | `ChatID` | 目标会话；新建必填，回复忽略 |
  | `Text` | 纯文本；与 Card 互斥 |
  | `Card` | 卡片 JSON；与 Text 互斥 |
  | `ReplyMessageID` | 非空走 reply 子路径 |

- **响应结构**（`messageData`，`internal/lark/rest.go:38-41`）：

  | 字段 | Go 类型 | 含义 |
  |------|---------|------|
  | `message_id` | string | 新消息 ID（外层 `data.message_id`） |
  | `chat_id` | string | 会话 ID（响应可携带，当前未消费） |

  外层信封 `imResponse{code,msg,data}`（`:30-34`）。

- **调用点**：REST 核心 `restClient.SendMessage`（`:46-75`）；底层 `doJSON`（`:110-154`）；公共出口 `Client.Send`（`internal/lark/client.go:136-138`）；业务封装 `Bot.SendCard`（`internal/feishu/bot_send.go:54-85`）、`Bot.SendText`（`:92-109`）。
- **上游业务用途**：
  - `SendCard`：backend/agent 流式结果渲染为交互卡片后发出；回复场景把卡片挂到用户消息下。
  - `SendText`：作为 `SendCard` 被飞书拒内容（§9.1）时的降级通道，保证回复不丢。
- **鉴权方式**：`Authorization: Bearer <tenant_access_token>`（`:127`）；`Content-Type: application/json; charset=utf-8`（`:128`）。
- **错误码处理**（`doJSON`，`:110-154`）：
  - HTTP ≥ 500 → `lark: <m> <p> http <code>: <body>`，不解析 JSON（`:138-140`）。
  - JSON decode 失败 → `lark: decode response: <err> (body: <截断>)`（`:142-143`）。
  - `code != 0` → `*APIError{Code,Msg}`（`:145-147`）。
  - `message_id` 空 → `SendMessage` 兜底 `lark: send returned no message_id`（`:71-73`）。
  - 读 body 上限 `1MiB`（`:134`）；HTTP 总超时 30s（`:160-162`）。
  - **不内置重试**；重试只在 UpdateCard。

---

## 3. 消息更新：PATCH 刷新卡片

- **分类**：消息更新
- **HTTP 方法**：`PATCH`
- **完整 URL**：`{baseURL}/open-apis/im/v1/messages/{message_id}`（无 query；`messageID` 经 `url.PathEscape`；`internal/lark/rest.go:83`）
- **请求结构**（`:84-85`）：

  | 字段 | Go 类型 | 含义 |
  |------|---------|------|
  | `content` | string | 新的卡片 JSON 字符串（cardkit 渲染产出） |

  请求头同 §2.1（Bearer + JSON）。

- **响应结构**：成功仅校验外层 `code == 0`；`doJSON` 传 `out=nil`（`:85`），不消费 `data`。
- **调用点**：REST 核心 `restClient.PatchMessage`（`:79-86`）；公共出口 `Client.PatchMessage`（`internal/lark/client.go:141-143`）；业务封装 `Bot.UpdateCard`（`internal/feishu/bot_send.go:113-167`，带退避重试 `:134-162`）、`Bot.updateFallbackCard`（`:171-177`）。
- **上游业务用途**：
  - 长任务进行中：进度卡片反复刷新。
  - 结果产出：placeholder 卡片替换为最终结果卡。
  - picker 交互：用户点击后等过 3-5s 静默回滚窗口再 PATCH（业务侧时序，不发 HTTP）。
- **鉴权方式**：同 §2.1。
- **错误码处理**（`Bot.UpdateCard`，`:134-162`）：
  - `isCardContentRejected(err)` → 短路 `updateFallbackCard`（极简卡片），**不重试**（`:140-145`）。
  - `IsCardGone(err)` → **不重试**，原样上抛 `feishu: update card (message gone): <w>`，让上游重发新卡（`:146-152`）。
  - 其他错误 → 退避重试，最多 `cardRetry=3` 次，初始 `cardRetryBase=300ms` 倍增（`:153-162`）。
  - ctx 取消 → 立即返回（`:158-159`）。

---

## 4. WebSocket 引导：拿连接 ticket

- **分类**：WebSocket 事件接收（引导阶段）
- **HTTP 方法**：`POST`
- **完整 URL**：`{baseURL}/callback/ws/endpoint`（常量 `BootstrapEndpoint` `internal/lark/ws/proto.go:43`；拼接 `wsclient.go:206`）
- **请求结构**（`internal/lark/ws/wsclient.go:204-211`）：

  | 字段 | Go 类型 | 含义 |
  |------|---------|------|
  | `AppID` | string | 自建应用 AppID（**PascalCase**，与 §1 的 snake_case 不同） |
  | `AppSecret` | string | 自建应用 AppSecret |

  请求头：`Content-Type: application/json`（`:210`）；`Locale: zh`（`:211`）。**无 Authorization 头**。

- **响应结构**（匿名 struct，`:220-232`）：

  | 字段 | JSON 路径 | Go 类型 | 含义 |
  |------|-----------|---------|------|
  | `code` | 顶层 | int | 0=OK |
  | `msg` | 顶层 | string | 错误描述 |
  | `URL` | `data.URL` | string | 签名 wss URL（含 `device_id`/`service_id` 等 query） |
  | `ClientConfig` | `data.ClientConfig` | struct（可空） | 服务器下发连接参数 |

  `ClientConfig`（`:225-230`）：`ReconnectCount`（int，-1=无限）、`ReconnectInterval`（秒）、`ReconnectNonce`（秒，首退避抖动上限）、`PingInterval`（秒）。

- **调用点**：HTTP 发送 `:212`；`Client.bootstrap` `:203-259`；上游 `Client.connect` `:177-199`；顶层 `Client.Start` `:129-158`。
- **上游业务用途**：WS 长连接握手第一步，换签名 URL + ping/reconnect 节奏；每次（重）连接都重跑。
- **鉴权方式**：Body 内 AppSecret。
- **错误码处理**（`:238-244`）：

  | code | 常量 | 处置 |
  |------|------|------|
  | 0 | `codeOK` | 正常 |
  | 514 | `codeAuthFailed` | 包成 `fatalError`（`:240-242`）；`Start` 检测 `isFatal` 后**立即返回不重试**（`wsclient.go:137-140`），让进程退出由 systemd 拉起，避免死循环 |
  | 1 | `codeSystemBusy` | 定义于 `proto.go:37`，未特判，按瞬态重连 |
  | 1000040343 | `codeInternalError` | 定义于 `proto.go:39`，同上 |
  | 其他 | — | 通用分支：非 fatal 错误，`Start` 走 `reconnectStep` 重连 |

  HTTP 非 200 → `ws: bootstrap http %d`（`:217-219`）；body 上限 `maxBootstrapBodyBytes = 1MiB`（`:20,235`）；`data.URL` 空 → 报错（`:245-247`）。

---

## 5. WebSocket 拨号（RFC 6455 升级握手）

- **分类**：WebSocket 事件接收（传输层）
- **HTTP 方法**：`GET`（带 `Upgrade: websocket`）
- **完整 URL**：§4 返回的 `data.URL`（含签名 query），由 `Client.connect` 透传（`wsclient.go:191`）
- **请求结构**（`internal/lark/websocket/dial.go:101-118`）：
  - 请求行：`GET <u.RequestURI()> HTTP/1.1`（含 query）
  - 必备头：`Connection: Upgrade`、`Upgrade: websocket`、`Sec-WebSocket-Version: 13`、`Sec-WebSocket-Key: <16字节随机 base64>`
  - 额外 header：调用方传 `nil`（`wsclient.go:191`），故无额外头
- **响应结构**：
  - `101 Switching Protocols`
  - 必须含 `Upgrade: websocket`（大小写不敏感）（`:145-149`）
  - `Sec-WebSocket-Accept` = `base64(sha1(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))`（`:150-155, 211-216`）
- **调用点**：`websocket.Dial`（`:42-161`）；拨号 seam `Client.dialer`（默认 `websocket.Dial`，`wsclient.go:62,95`）；调用 `wsclient.go:191`。
- **上游业务用途**：把 §4 的 ticket 升级为 WS 长连接。
- **鉴权方式**：URL 内签名 query（飞书签发）；客户端不加 token。
- **错误码处理**：
  - 非 101 → `websocket: server returned %d, expected 101`（`:140-144`）。
  - Upgrade 头缺失/不匹配 → 报错关 conn（`:145-149`）。
  - Accept 不匹配 → 报错关 conn（`:150-155`）。
  - TLS/NET 拨号失败 → `websocket: dial: %w`（`:88-90`）；上游包装 `ws: dial: %w`（`wsclient.go:196`）。

---

## 6. WebSocket 收事件：`im.message.receive_v1`

- **分类**：WebSocket 事件接收（数据帧）
- **WS 帧类型**：Binary（`OpcodeBinary=0x2`），`Method=MethodData=1`，`Headers.type=TypeEvent="event"`（`internal/lark/ws/proto.go:9-11,28`）
- **帧结构**（protobuf `Frame`，`internal/lark/ws/frame.go:24-34`）：

  | 字段 | Protobuf 字号 | Go 类型 | 含义 |
  |------|----------------|---------|------|
  | `SeqID` | 1 varint | uint64 | 帧序号；ACK 必须回填 |
  | `LogID` | 2 varint | uint64 | 日志 ID；ACK 必须回填 |
  | `Service` | 3 varint | int32 | 服务 ID；ping 帧用 |
  | `Method` | 4 varint | int32 | 0=control，1=data |
  | `Headers` | 5 repeated | []Header | KV（type/message_id/sum/seq/timestamp/trace_id/instance_id/biz_rt） |
  | `PayloadEncoding` | 6 string | string | 可选 |
  | `PayloadType` | 7 string | string | 可选 |
  | `Payload` | 8 bytes | []byte | 业务 JSON |
  | `LogIDNew` | 9 string | string | 新版日志 ID |

  Headers 常量：`proto.go:14-24`。

- **Payload 业务 JSON**——v2 信封（`envelope`，`internal/lark/ws/dispatcher.go:149-155`）：

  | JSON 路径 | 类型 | 含义 |
  |-----------|------|------|
  | `header.event_id` | string | 事件 ID |
  | `header.event_type` | string | 必须为 `"im.message.receive_v1"` 才进本分支 |
  | `event` | raw | 事件体，下表 |

  `event` 体（`receiveEvent`，`:186-209`）：

  | JSON 路径 | 类型 | 含义 |
  |-----------|------|------|
  | `event.sender.sender_id.open_id` | string | 发送者 open_id |
  | `event.message.message_id` | string | 消息 ID |
  | `event.message.create_time` | string | unix-ms 字符串 |
  | `event.message.chat_id` | string | 会话 ID |
  | `event.message.chat_type` | string | p2p/group |
  | `event.message.message_type` | string | text/image/... |
  | `event.message.content` | string | 内容 JSON（text 时 `{"text":"..."}`） |
  | `event.message.mentions[].key` | string | `@_user_1` 占位符 |
  | `event.message.mentions[].name` | string | 显示名 |
  | `event.message.mentions[].id.open_id` | string | 被提及者 open_id |
  | `event.message.mentions[].mentioned_type` | string | `"app"` 表示 @ 的是本机器人 |
  | `event.message.mentions[].is_bot` | bool | 新字段，可缺失 |

- **"响应"**：入站事件，响应即 §8 的 ACK 帧。
- **调用点 / 解析点**：帧解码 `Frame.Unmarshal`（`frame.go:155-285`）；路由分支 `dispatcher.go:168-173`；payload 解析 `parseMessageReceive`（`:212-249`）；透传 `Sink.OnMessage`（`:173`）；adapter `handlerSinkAdapter.OnMessage`（`client.go:198-210`）；业务消费 `Bot.handleMessageReceive`（`internal/feishu/bot_dispatch.go:19-84`）。
- **上游业务用途**：接收用户消息，归一化为 `IncomingMessage`（`internal/feishu/bot.go:27-47`），交给 `IncomingHandler` 路由到后端。
- **鉴权方式**：WS bootstrap 已鉴权；帧无额外凭据。
- **错误码处理**：
  - 帧解码失败：`fireError` + continue（不 ACK，不结束 session）（`session.go:101-104`）。
  - 信封/事件 JSON 解析失败：`dispatch` 返回 err → ACK 500（`dispatcher.go:164-165,178-181`）。
  - 业务 handler 返回 err/panic：ACK 500（`bot_dispatch.go:57-78`）。
  - ACK 500 后飞书重投，由 `internal/feishufront/dedup.go` 去重。

---

## 7. WebSocket 卡片回调：`card.action.trigger`

- **分类**：卡片回调（WS 入站）
- **WS 帧类型**：同 §6
- **帧结构**：同 §6
- **Payload 结构**：v2 信封同 §6，但 `header.event_type == "card.action.trigger"`，`event` 体为 `cardEvent`（`dispatcher.go:252-264`）：

  | JSON 路径 | 类型 | 含义 |
  |-----------|------|------|
  | `event.operator.open_id` | string | 点击者 open_id |
  | `event.action.value` | map[string]any | 按钮 value（非表单按钮点击） |
  | `event.action.form_value` | map[string]any | 表单提交值（按组件 name 索引） |
  | `event.context.open_message_id` | string | 触发点击的卡片消息 ID |
  | `event.context.open_chat_id` | string | 会话 ID |

- **"响应"**：见 §8 ACK。当前 ACK 仅 `{"code": <200|500>}`（曾尝试在 ACK 带业务字段规避 3s 回滚，commit `53ea21d` 判定无效并移除）。
- **调用点 / 解析点**：路由分支 `dispatcher.go:174-179`；解析 `parseCardAction`（`:267-282`）；透传 `Sink.OnCard`（`:179`）；adapter `handlerSinkAdapter.OnCard`（`client.go:212-223`）；业务消费 `Bot.handleCardAction`（`bot_dispatch.go:90-127`）。
- **上游业务用途**：处理 picker 选择、abort、config 等交互按钮点击。
- **鉴权方式**：WS bootstrap 已鉴权。
- **错误码处理**：
  - `operator.open_id` 空：丢弃（`bot_dispatch.go:110-113`）。
  - handler panic：recover → ACK 500（`:114-126`）。
  - 解析失败：ACK 500（同 §6）。

---

## 8. WebSocket ACK 帧（事件/卡片回执）

- **分类**：WebSocket 事件接收（出站控制）
- **WS 帧类型**：Binary，结构为「克隆入站 data 帧的 Headers + 替换 Payload」（`NewAckFrame`，`frame.go:95-107`）
- **Payload**（`session.go:193`）：`{"code": <ackCode>}`
  - `200`：handler 成功或 sink 为 nil（`:174-175,182`）
  - `500`：dispatch / handler 返回 err（`:178-181`）
- **调用点**：构造与写入 `writeAck`（`:192-202`）；触发于 `handleData` 重组完成（`:174,182`）；写失败 `fireError("ws: write ack: ...")`（`:200-201`）。
- **上游业务用途**：飞书要求每个 data 帧必须 ACK，否则视为投递失败并重投（注释 `:159-162`）。
- **错误码处理**：未 ACK 或 500 → 飞书重投 → 上游 `dedup.go` 去重。

---

## 9. WebSocket Ping 心跳

- **分类**：WebSocket 事件接收（出站控制）
- **WS 帧类型**：Binary，`Method=MethodControl=0`，`Headers.type=TypePing="ping"`，`Service=c.serviceID`（`NewPingFrame`，`frame.go:84-90`）
- **Payload**：空
- **调用点**：循环 `pingLoop`（`session.go:209-232`）；间隔 `c.cfg.PingInterval`，每次 tick 重读，默认 90s（`:211-217`）；写失败 `return` 结束 session 触发重连（`:228-230`）。
- **上游业务用途**：保活；服务器回 pong，可能携带新 `ClientConfig`（见 §10）。
- **错误码处理**：写失败即结束 session；无业务码。

---

## 10. WebSocket Pong（服务器→客户端，配置刷新）

- **分类**：WebSocket 事件接收（入站控制）
- **WS 帧类型**：Binary，`Method=MethodControl=0`，`Headers.type=TypePong="pong"`
- **Payload 结构**（可选；空则忽略）（`handleControl`，`session.go:132-157`）：

  | 字段 | Go 类型 | 含义 |
  |------|---------|------|
  | `ReconnectCount` | int | -1=无限 |
  | `ReconnectInterval` | int | 秒 |
  | `ReconnectNonce` | int | 秒 |
  | `PingInterval` | int | 秒 |

- **调用点**：`handleControl`（`:132-157`）；`receiveLoop` 对 `MethodControl` 帧分发（`:106-107`）。
- **上游业务用途**：服务器在不断线前提下调整 ping/reconnect 节奏。
- **错误码处理**：非 pong 帧 / 解析失败 → 静默忽略（`:134-136,146-148`）。

---

## 11. WebSocket 重连与心跳超时机制

### 11.1 主循环
`Client.Start`（`wsclient.go:129-158`）：`connect → runSession → reconnectStep` 循环。`fatalError`（如 bootstrap 514）立即返回不重试（`:137-140`）；`isFatal` 判定 `reconnect.go:50-58`。

### 11.2 重连预算与退避
- `reconnectStep`（`wsclient.go:163-172`）：`ReconnectCount >= 0` 按次数封顶；`-1` 无限。健康 session 重置计数（`:144`）。
- `reconnectSleep`（`reconnect.go:14-31`）：先 `[0, ReconnectNonce)` 抖动，再睡 `ReconnectInterval`；ctx 取消返回 false。
- 默认值（bootstrap 未下发前）`defaultConfig`（`wsclient.go:75-82`）：`ReconnectCount=-1`、`ReconnectInterval=90s`、`ReconnectNonce=25s`、`PingInterval=90s`。

### 11.3 读超时（半开检测）
- `refreshReadDeadline`（`session.go:117-125`）：每次成功读到帧后把读 deadline 推 `2 * PingInterval`。
- 触发于 `receiveLoop` 入口与每次 `ReadMessage` 后（`:85,96`）。
- 设计意图（`:76-82`）：两个 ping 周期无任何帧 → 读超时 → `receiveLoop` 退出 → 关 conn → 重连。补齐原 SDK 缺失的 `SetReadDeadline`，避免半开连接只能等 5 分钟看门狗才发现。

### 11.4 生命周期回调
- 触发 `fireReady/Error/Reconnecting/Disconnected`（`reconnect.go:63-86`）。
- `OnDisconnected` 仅在非 ctx 取消时触发（`wsclient.go:146-152`），避免优雅 Stop 被误判为抖动。
- 业务侧 wiring `Bot.lifecycle`（`internal/feishu/bot.go:187-207`）：更新 `lastHealthy` 看门狗 + 打日志。

### 11.5 chunk 重组
- `reassembler`（`dispatcher.go:65-140`）：按 `message_id`/`sum`/`seq` 重组分片 data 帧。
- 上限 `maxReassembleChunks = 256`（`:88`），超限丢弃。
- 过期清理 `chunkTTL = 5s`，由 `runSession` 第三个 goroutine 定时 sweep（`session.go:27,46-56,131-140`）。

---

## 12. 错误码识别专题

### 12.1 `isCardContentRejected` —— 卡片内容被拒
- 位置：`internal/feishu/bot_send.go:185-193`
- 触发码（任一子串命中）：
  - `code:230025` —— `feishuCodeContentTooLarge`（`:39`），body 字节超限
  - `code:11310` —— `feishuCodeCardElementOverLimit`（`:45`），表格/元素数量超限（通常裹在 230099 内层）
  - `"over limit"` —— 兜底英文短语
- 处置：
  - `SendCard`：返回 `ErrCardContentRejected`（包装），调用方决定降级（`:68-77`）；**不自动发 stub**（`:71-73` 注释说明旧逻辑已移除）。
  - `UpdateCard`：短路 `updateFallbackCard`，重 PATCH 极简卡片（`:140-145,171-177`；卡片构造 `:218-253`）。

### 12.2 `IsCardGone` —— 卡片已不可 PATCH
- 位置：`internal/feishu/bot_send.go:203-209`
- 触发码（子串）：
  - `code:230011` —— 消息被撤回（已在生产 API 实测验证，注释 `:196-202`）
  - `code:99992354` —— message_id 无效/不存在（防御性）
- 处置：
  - `UpdateCard`：**不重试**，原样上抛（`:146-152`）。
  - 跨包消费：`internal/feishufront/dispatcher_control.go:364`（`if !feishu.IsCardGone(err)`），用于把失效 messageID 从缓存剔除并重发新卡（status-monitor 广播路径）。

---

## 13. Protobuf 帧编解码位置汇总

| 项 | 位置 |
|----|------|
| `Frame` 结构与字段字号注释 | `internal/lark/ws/frame.go:9-34` |
| `Header` 结构 | `internal/lark/ws/frame.go:38-41` |
| `Headers` 线性查找 helper | `internal/lark/ws/frame.go:48-73` |
| `Marshal`（手写 protobuf 编码器） | `internal/lark/ws/frame.go:113-149` |
| `Unmarshal`（手写解码器，跳过未知字段） | `internal/lark/ws/frame.go:155-285` |
| varint/length-delimited 原语 | `internal/lark/ws/frame.go:363-422` |
| `skipField`（未知字段跳过） | `internal/lark/ws/frame.go:325-352` |
| 头部上限 `maxFrameHeaders = 64` | `internal/lark/ws/frame.go:46` |
| Method / Type 常量 | `internal/lark/ws/proto.go:8-32` |

仓库**不依赖 protoc 生成代码**，整个 protobuf 编解码是手写的、与官方 SDK（gogo-proto）线格式兼容的最小实现。

---

## 14. 域名与配置入口

- `resolveBaseURL`（`internal/lark/client.go:37-48`）：
  - `""` / `"feishu"` → `https://open.feishu.cn`
  - `"larksuite"` / `"lark"` → `https://open.larksuite.com`
  - 含 `://` → 原样去尾斜杠
  - 其他裸 host → `https://<host>`
- 默认值：`NewClient` 内 `resolveBaseURL("feishu")`（`:82`）
- 业务侧透传：`feishu.WithDomain` → `lark.WithDomain`（`internal/feishu/bot.go:110-112,164-173`）；默认 `botConfig.Domain = "feishu"`（`:177`）
- 三个 baseURL 使用点（`NewClient` 用同一 `cfg.baseURL` 构造，`:87-91`）：
  - REST：`restClient.baseURL`（`internal/lark/rest.go:23`）
  - Token：`tokenManager.baseURL`（`internal/lark/auth.go:30`）
  - WS bootstrap：`Client.baseURL`（`internal/lark/ws/wsclient.go:40`）

---

## 15. 已定义但实际未特判 / 未使用的符号

| 符号 | 位置 | 状态 |
|------|------|------|
| `codeSystemBusy = 1` | `internal/lark/ws/proto.go:37` | 定义但 bootstrap 未特判，按通用瞬态错误重连 |
| `codeInternalError = 1000040343` | `internal/lark/ws/proto.go:39` | 同上 |
| `queryDeviceID = "device_id"` | `internal/lark/ws/proto.go:48` | 定义但未读（仅 `queryServiceID` 被使用，`reconnect.go:103-113`） |
| `TypeCard = "card"` | `internal/lark/ws/proto.go:29` | 注释明示 "legacy card channel (unused by this app)" |
| `Conn.WritePing` | `internal/lark/websocket/conn.go:341-343` | 仅协议完整性/测试；生产 ping 走 `WriteMessage(Binary, protobuf 帧)` |
| `Conn.Underlying` | `internal/lark/websocket/conn.go:89` | 注释："lark-bridge uses it for nothing in production" |
| `WithLogLevel` | `internal/lark/client.go:30-32` | 仅设 `cfg.logLevel`，注释明示 reserved，客户端本身不打日志 |
| `messageData.ChatID` | `internal/lark/rest.go:40` | 响应字段定义但当前未消费（只读 `MessageID`） |

---

## 附：调用链速查（自上而下）

```
feishu.NewBotWithLogger                                   internal/feishu/bot.go:120
  └─ lark.NewClient                                       internal/lark/client.go:78
       ├─ tokenManager{baseURL,http}                      internal/lark/client.go:87
       ├─ restClient{baseURL,http,tokens}                 internal/lark/client.go:88
       └─ ws.New(appID,appSecret,baseURL,httpc)           internal/lark/client.go:91 → ws/wsclient.go:103

Bot.Start → client.Start → ws.Start                       internal/feishu/bot.go:212 → lark/client.go:104 → ws/wsclient.go:129
  └─ connect                                              ws/wsclient.go:177
       ├─ bootstrap  POST /callback/ws/endpoint           ws/wsclient.go:203-259
       └─ websocket.Dial(GET Upgrade)                     ws/wsclient.go:191 → websocket/dial.go:42
  └─ runSession                                           ws/session.go:22
       ├─ receiveLoop                                     ws/session.go:83
       │    ├─ Frame.Unmarshal                            ws/frame.go:155
       │    ├─ handleControl (pong→刷新 cfg)              ws/session.go:132
       │    └─ handleData                                 ws/session.go:162
       │         ├─ reassembler.feed                      ws/dispatcher.go:97
       │         ├─ router.dispatch                       ws/dispatcher.go:159
       │         │    ├─ im.message.receive_v1 → Sink.OnMessage   ws/dispatcher.go:168
       │         │    └─ card.action.trigger    → Sink.OnCard     ws/dispatcher.go:174
       │         └─ writeAck                              ws/session.go:192
       └─ pingLoop (NewPingFrame → WriteMessage)          ws/session.go:209

Bot.SendCard/SendText → client.Send → restClient.SendMessage → doJSON → tokens.Token
                                                          internal/feishu/bot_send.go:54,92
                                                          internal/lark/client.go:136
                                                          internal/lark/rest.go:46,110
                                                          internal/lark/auth.go:59
Bot.UpdateCard → client.PatchMessage → restClient.PatchMessage → doJSON
                                                          internal/feishu/bot_send.go:113
                                                          internal/lark/client.go:141
                                                          internal/lark/rest.go:79
```
