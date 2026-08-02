# 项目规则

## 编码规范

详见 [CODING_STANDARDS.md](../CODING_STANDARDS.md)。

## 架构

详见 [ARCHITECTURE.md](../ARCHITECTURE.md)。

## 注意事项

- Go 1.25+，直接依赖仅标准库，无第三方模块。
- 飞书协议由 `internal/lark/` 自实现（RFC 6455 WebSocket + 手写 protobuf）。
- 多后端共享 config 文件，各进程只读自己需要的字段。
- 敏感数据通过环境变量注入，不落盘 JSON。
