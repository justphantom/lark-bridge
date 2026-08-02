# Miniagent 项目级记忆
#
# 此目录包含 miniagent 项目级配置：
#
# - memory.jsonl   项目记忆（由 write(path="memory") 工具写入，/memory 命令读取）
# - persona.md     项目 persona（注入 system prompt）
# - rules.md       项目规则（注入 system prompt）
#
# 文件会被合并进 miniagent 的 system prompt（见 project.go）。
# .gitignore 忽略记忆文件（含敏感信息），保留模板。
