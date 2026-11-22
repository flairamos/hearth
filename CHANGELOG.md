# Changelog

## 0.4.1 — 2026-11-22

### 新增
- **浏览器 GUI**：`hearth --web` 在 localhost:8787 起本地 Web 界面（流式 Markdown 对话、会话侧栏、斜杠命令、@ 文件引用、agent 审批对话框、ctx/tps 指标条），仅监听 127.0.0.1
- **服务商档案 P1**：config.json `providers` 配置 GLM / DeepSeek / Qwen / 方舟等 OpenAI 兼容端点；`/provider` 一键切换（本地 ↔ 云端）；`api_key` 支持 `$环境变量` 引用；`hearth-agent -provider`
- Web 端斜杠命令（/new /clear /compact /model /provider /system /temp /ctx /think /agent /title）、@ 文件选择器与 #L 行号引用

### 修复
- Windows：glob 工具输出统一正斜杠；PowerShell 输出强制 UTF-8 + GBK 回退解码（中文不再乱码）；权限测试平台无关化
- Web：thinking 增量累加（不再逐 token 竖排）；/compact 行数截断（CJK 总结不再被肢解）；chat 路径 thinking 回退重试；resize 后强制全屏重绘（残影）
- TUI：agent 模式 @ 文件引用注入（此前被静默丢弃）；/quit 立即退出并保存会话；/clear 语义 = 删除会话
- 会话列表空会话隐藏（面板/侧栏一致）；Web @ 选择器键盘导航

### 工程
- CI：push/PR 自动跑 gofmt/vet/race 测试（ubuntu+windows）/staticcheck/6 目标编译
- 版本号构建注入（-ldflags -X，git tag 为准）；MIT LICENSE；CHANGELOG

## 0.4.0 — 基线

- 双二进制（TUI + 无头 agent）、13 个内置工具、双 API 风格（Ollama 原生 + OpenAI 兼容）、后端自动发现、自动压缩、双层记忆、多模态、GitHub Pages 文档站
