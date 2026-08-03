# Miniagent 项目级记忆
#
# 此目录包含 miniagent 项目级配置：
#
# - memory.jsonl   项目记忆（由 write(path="memory") 工具写入，/memory 命令读取）
# - persona.md     项目 persona（注入 system prompt）
# - rules.md       项目规则（注入 system prompt）
# - scripts.json   项目脚本（每条注册为 script_<name> 工具，见 tool_script.go）
#
# 加载优先级：workdir/.miniagent/ > ~/.miniagent/，各文件单独覆盖（见 project.go）。
# persona.md / rules.md / scripts.json 合并进 system prompt；memory.jsonl 由会话结束自动追加。
# .gitignore 忽略记忆文件（含敏感信息），保留模板。
