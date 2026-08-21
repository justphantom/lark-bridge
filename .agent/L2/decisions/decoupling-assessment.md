---
layer: L2
type: decision
tags: [decoupling, dependency-graph, config-split, interface-seam, bridgebase, feishufront, refactor-campaign]
created: 2026-08-19
confidence: high
verified_at: 2026-08-19
applies_to: HEAD (P0-P3 全交付)
---

# 解耦程度评估与重构战役决策

## 背景
用户要求深入评估项目解耦程度。基于 `go list` 权威非测试 import 图 + 关键接缝核查（bridgebase 消费者、router 语义、config 结构、protocol/backendrpc 契约面）得出结论，并拍板四阶段重构战役。

## 评估结论：整体良好
强隔离在关键缝上是有意识设计的结果；债务集中两处（config 单体、bridgebase 虚共享层）。

## 依赖图（严格 DAG，无环）
```
叶(0依赖)   protocol log strutil cmdutil eventmetrics atomicwrite
中          gosafe→log  streamarchive→log  config→strutil
            lark→lark/ws→lark/websocket  fileconvert→strutil  router→atomicwrite,log
高          backendrpc→gosafe,hostmetrics,log,protocol ★传输层
            feishu→lark,log  feishufront/renderer→cardkit,protocol
            bridgebase→backendrpc,cmdutil,eventmetrics,gosafe,log,protocol
            miniclient→bridgebase/linereader,cmdutil,eventmetrics,log
顶          feishufront→feishu,cardkit,renderer,fileconvert,protocol,…
            miniagent→bridgebase,backendrpc,miniclient,router,protocol,…
            statusmonitor→backendrpc,protocol
装配        cmd/{feishu-front,miniagent-back,status-monitor}→config+各自域包
```

## 强项（按含金量）
1. **外部依赖零**：go.mod 仅 module 行 + go 1.25.0，无 go.sum，飞书 SDK/WS/SSE 全自实现。
2. **前后端隔离最强缝**：feishufront 的 import 零后端代码（无 backendrpc/miniagent/miniclient），只认 protocol 契约 + registry 动态注册（BackendID/BackendType 字符串）。新增第三种后端前端零改动——agnes/claude 两次整体移除未碰 feishufront 即证明。
3. **传输层接口化**：backendrpc.ControlSender/StatusQuerier 接口 + `var _ ControlSender = (*Client)(nil)` 编译期断言；miniagent 用 `type controlSender = backendrpc.ControlSender` 别名持接口。
4. **后端对飞书零反向依赖**：miniagent 不 import feishu/lark/feishufront。
5. **反环被文档化守护**：feishufront/dedup.go:117 显式注明"backendrpc 测试用 feishufront 做 fixture，故 feishufront 不得 import bridgebase"。

## 债务（按修复价值）
1. **config 单体**：三服务共享同一 union struct（feishu 凭证+IPC+TLS+miniagent+status_monitor+日志…），字段归属仅靠注释约定。加字段三服务全重编译；DisallowUnknownFields 下字段演进是全局事件。deploy 拆了 3 文件但类型层没拆。
2. **bridgebase 虚共享层**：1832 行"共享助手层"，agnes/claude 删除后消费者只剩 miniagent（7 文件 import）。唯一真跨切面接缝 EmitTerminalControl 因接口定义在 backendrpc 包，拉出 bridgebase→backendrpc 整包依赖 + feishufront 反环约束。
3. **feishufront 巨包**（✅已部分缓解 2026-08-19）：IPC 传输拆出 `feishufront/ipcserver/` 子包，与后端 `backendrpc` 对称；`cardkit`/`renderer` 此前已拆。dispatcher 主体（与 registry/turn/routing/dedup 同一内聚类）刻意保留——拆 dispatcher 只搬代码不降耦合（P3 评估结论）。
4. **protocol 胖契约**（可接受）：~780 行非测试，单一契约包合理形态，不算真债。
5. **库化 N/A**：全在 internal/ 下——应用定位（3 binaries）无外部复用场景，正确选择而非债。

## 评分
外部依赖★★★★★ / 前后端隔离★★★★★ / 传输-业务接缝★★★★☆ / 配置隔离★★☆☆☆ / 共享层抽象★★☆☆☆ / 包粒度★★★☆☆

## 决策：四阶段重构战役（用户拍板顺序）
| 阶段 | 内容 | 关键点 |
|---|---|---|
| P0 | 本评估沉淀 L2 | 即本文件 |
| P1 | config 拆分 + 接口迁移 | ✅已交付：按服务 Load（owned 键过滤+联合 known-set 拒顶层 typo，D5 放宽）；ControlSender/StatusQuerier 迁 protocol |
| P2 | bridgebase 并入 miniagent | ✅已交付：原 bridgebase/* 并入 internal/miniagent（sendfile.go 因文件名冲突改名），linereader 升顶层 internal/linereader（miniclient 唯一消费者，随入会成环），bridgebase→backendrpc 依赖随之消失 |
| P3 | feishufront 子包化 | ✅已交付（范围收窄为 ipcserver only）：`feishufront/ipcserver/` 子包拆出（与 backendrpc 对称的传输边界）；实测 ipcserver 对 Dispatcher **零类型依赖**（callback 反向接线），仅依赖 BackendRegistry/Turn/BackendConn——与建议 B 全拆 dispatcher 相比：dispatcher 与支撑类型（registry/turn/routing/dedup）是同一内聚类，拆它只搬代码不降耦合，故弃。需导出 3 处（ConnSnapshot/HostDedupKey + Events() 访问器 + Dispatcher 4 个测试钩子）+ e2e 测试随迁 ipcserver |

每阶段独立提交，build/vet/lint/test 全绿，ARCHITECTURE.md 与 L2 同步。

## 参考
- 接缝核查证据：`go list -f ... ./internal/...`；bridgebase 消费者 grep；feishufront/dedup.go:117 反环注释
