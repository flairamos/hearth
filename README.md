# hearth — 炉边

**把炉火留在自己的机器上。**

Hearth 是运行在终端里的本地大模型工作台：交互式对话 + 无头 Agent 双二进制，零云端依赖、键盘驱动、专为本地小参数量化模型调优。命名取自「炉边」—— 模型如炉火，常暖常驻（`keep_alive` 让模型不熄火），数据不出家门。

> **命名理由**：hearth = 家中的炉火。它不是又一个 "llm-cli"，而是一张炉边座位：模型在你自己的硬件上常驻发热（keep_alive），对话与自动化都围炉而行。对标 opencode 的产品形态，但能源来自你自己的 GPU。

| 二进制 | 定位 | 大小 |
|---|---|---|
| `hearth` | 交互式 TUI：对话、会话、`@`引用、agent 模式（审批 UI）、多模态、性能仪表 | ~21MB |
| `hearth-agent` | 无头自动化：`agent` / `ask` / `models`，脚本与 CI 友好，**零 TUI 依赖** | ~11MB |

## 双 API 风格（多后端自动识别）

同一个客户端支持两种 API 风格，`api_style: "auto"` 按 URL 自动识别；**配置地址失效时自动探测常见本地端口**（Ollama :11434 / LM Studio :1234 / llama.cpp :8080 / vLLM :8000 / Jan :1337），找到后写回配置：

| 风格 | 地址 | 适用 |
|---|---|---|
| `ollama`（原生） | `http://localhost:11434` | thinking 字段完整支持、keep_alive、能力探测 |
| `openai` | `http://localhost:11434/v1` 等 | LM Studio、llama.cpp server、vLLM 等一切 OpenAI 兼容端点 |

## Agent 工具集（13 个）

| 类别 | 工具 | 审批（access 三档） |
|---|---|---|
| 读取 | `read_file` `list_dir` `glob` `grep` | 自动放行 |
| 计算 | `calculator`（纯 Go 表达式求值） | 自动放行 |
| 记忆 | `note`（会话草稿本）· `memory_recall` | 自动放行 |
| Web | `web_fetch` · `web_search`（Bing / DuckDuckGo / SearXNG / Brave） | 自动放行 |
| 记忆 | `memory_save`（跨会话持久化） | ask: 审批 · risky/full: 自动 |
| 修改 | `write_file` `edit_file`（唯一性校验 + CRLF 安全） | ask: 审批 · risky: 工作目录内自动 |
| 执行 | `bash`（Windows 自动选 PowerShell / Git Bash / cmd） | ask: 审批 · risky: 仅只读白名单自动 |

**access 权限三档**（`/access ask|risky|full` 或 `Ctrl+G` 循环，header 徽章实时显示）：
- **ask** —— 变更前询问
- **risky** —— 有风险才询问（安全命令白名单 + 工作目录内写入自动放行）
- **full** —— 完全自主；危险命令（rm -rf、sudo、`curl|sh`、`del /s`、`format C:` …）**任何档位强制人工确认**

**记忆体系**：短期 = 会话消息 + `note` 草稿本（每步注入，模型始终可见自己的计划）；长期 = `memory.md` 事实库（跨会话跨进程，每回合注入最近 8 条）。

**文件引用**：输入 `@` 打开模糊文件选择器；`@path#L121-135` 附加行范围；发送时快照内容注入上下文。

**自动压缩**：token 达 85% 时回合结束后自动摘要压缩，保留「摘要 + 尾部原文 + 草稿本 + 长期记忆」，长对话不断联。

## 安装（Windows 优先）

**Windows（Windows Terminal 推荐）：**

```powershell
# 方式一：源码构建（产出 zip + sha256 到 dist\<版本>\）
git clone https://github.com/firea/hearth && cd hearth
.\scripts\build.ps1            # 加 -All 构建全平台矩阵

# 方式二：把构建产物放进 PATH
Copy-Item dist\0.4.1\*.exe $env:USERPROFILE\bin\

# 方式三：Scoop（发布后）
scoop bucket add hearth https://github.com/firea/hearth
scoop install hearth           # 同时安装 hearth 与 hearth-agent
```

**Windows 启动**（Release 的 `*_windows_amd64.zip` 内含全部所需文件）：

- **双击 `run-tui.cmd`** —— 自动打开 Windows Terminal 并启动界面；`run-agent.cmd` 打开 agent 用法说明
- ⚠️ 认准 `*_windows_*.zip` 里的 `.exe` —— linux/macOS 产物没有扩展名，双击会弹"选择打开方式"
- SmartScreen 拦截时：`更多信息 → 仍要运行`，或 `Unblock-File .\hearth.exe`
- 双击 exe 遇错时窗口会保留（press Enter to close）；最常见原因是 Ollama 未启动 —— 先运行 `ollama serve`

**Linux / macOS：**

```bash
git clone https://github.com/firea/hearth && cd hearth
./scripts/build.sh
sudo cp dist/0.4.1/hearth{,-agent}_linux_amd64 /usr/local/bin/
```

**启动**（内置后端自动发现）：

```powershell
ollama pull qwen3.5:4b
hearth                          # 交互式 TUI
hearth-agent agent "统计行数" --yolo     # 无头自动化
hearth-agent ask "一句话解释 TCP"        # 单问
```

**配置**：`~/.config/hearth/config.json`（Windows: `%AppData%\hearth\config.json`）。**旧版 ollama-tui 的配置与会话自动沿用，无需迁移**。完整示例见 [`configs/config.example.json`](configs/config.example.json)；TUI 内 `/config` 热重载。

关键项：

| 项 | 默认 | 说明 |
|---|---|---|
| `api_style` | `auto` | `auto` / `ollama` / `openai` |
| `discovery` | `true` | 后端自动探测 |
| `context.keep_alive` | `30m` | 模型常驻（炉火不熄） |
| `context.auto_compact` | `true` | 85% 自动摘要压缩 |
| `agent.permissions` | `ask` | 三档 `ask` / `risky` / `full` |
| `agent.shell` | `auto` | `auto` / `bash` / `powershell` / `cmd` |
| `agent.memory` | `true` | 长期记忆 |
| `search.engine` | `bing` | `bing` / `duckduckgo` / `searxng` / `brave` / `none` |

## 文档站点

完整文档以静态站点形式放在 [`docs/`](docs/)（首页 / 安装 / 使用指南 / hearth-agent / 配置参考 / 架构），通过 GitHub Pages **「Deploy from a branch」** 模式发布：Settings → Pages → Source 选 `Deploy from a branch` → 分支 `main`、文件夹 `/docs`。部署步骤见 [`docs/DEPLOY.md`](docs/DEPLOY.md)，站点地址 `https://<用户名>.github.io/hearth/`。

## 快捷键

| 键 | 功能 | | 键 | 功能 |
|---|---|---|---|---|
| `Enter` | 发送 | | `Alt+A` / `Shift+Tab` | Agent 模式开关 |
| `Alt+Enter` / `\`+`Enter` | 换行 | | `Ctrl+T` | 思考模式开关 |
| `Esc` | 中断生成 | | `Ctrl+B` | 侧栏开关 |
| `Ctrl+K` | 命令面板 | | `Ctrl+L` | 清屏（保留上下文） |
| `Ctrl+P` | 会话切换 | | `?` | 帮助 |
| `Alt+M` | 模型切换 | | `Ctrl+C`×2 | 退出（生成中=中断） |
| `Ctrl+N` | 新会话 | | `Ctrl+G` | access 权限档循环 |
| `@` | 文件模糊选择器 | | `Ctrl+Y` | 复制最后回复 |
| `↑/↓` | 输入历史 | | `PgUp/PgDn` | 滚动（End 回底部） |

## 架构

```
cmd/hearth           TUI 入口（独立安装）
  cmd/hearth-agent    无头入口（独立安装，零 TUI 依赖，~11MB）
internal/
  tui/                Bubble Tea 界面（状态机、面板、审批对话框、渲染缓存）
  agent/              agent 循环 + access 权限管理器
  tools/              13 个内置工具（纯 Go，跨平台）
  ollama/             Ollama 原生客户端（NDJSON 流式、断线退避重试、错误分类）
  provider/           API 风格工厂 + 本地后端自动发现
  core/               Session/Message 领域模型（并发安全）、token 估算、裁剪
  store/              会话持久化（JSON、原子写）
  config/             配置加载/保存（旧品牌路径无缝迁移）
  theme/              Lipgloss 主题
```

## 性能（实测 qwen3.5:4b / RTX 4050）

| 场景 | 吞吐 |
|---|---|
| 裸 curl 基线 | 53.8 tok/s |
| 本工具单轮对话 | **53.6 tok/s（99.6% 上限）** |
| 多步 agent（4 次工具调用） | **53.1 tok/s**，keep_alive + 前缀缓存优化后壁钟 -22% |

优化手段：`keep_alive` 模型常驻（炉火不熄）；易变上下文（草稿本/记忆）注入移到消息尾部以保住 Ollama 前缀缓存；50ms 批量渲染 + 完成消息渲染缓存。

## 测试

```bash
go test -race ./...    # 单元测试（agent 循环 mock 回放、SSE 分片、CRLF、并发压力）
go vet ./...
GOOS=windows go build ./cmd/hearth ./cmd/hearth-agent
```

## Roadmap

- [x] M1–M4：对话/会话/快捷键/命令面板/Markdown/主题/斜杠命令/上下文管理/多模态/Agent+审批/双 API 风格/记忆/web 工具/自动发现/自动压缩
- [x] 产品拆分：hearth + hearth-agent 独立安装
- [ ] M5：MCP 客户端（Registry 接口已预留）、FTS5 会话搜索、RAG
