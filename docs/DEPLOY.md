# GitHub Pages 部署说明（Deploy from a branch）

本目录（`docs/`）是 hearth 的静态文档站点，采用 GitHub Pages 最简单的
**「Deploy from a branch」** 模式部署 —— 无需 Jekyll 构建、无需额外分支、
无需 CI，提交即发布。

## 部署步骤

1. 把 `docs/` 目录提交并推送到 `main` 分支：

   ```bash
   git add docs/
   git commit -m "docs: add GitHub Pages site"
   git push
   ```

2. 打开 GitHub 仓库页面 → **Settings** → 左侧 **Pages**：

   - **Source**: 选择 `Deploy from a branch`
   - **Branch**: 选择 `main`，文件夹选择 **`/docs`**
   - 点击 **Save**

3. 等待 1–2 分钟构建，访问：

   ```
   https://<你的用户名>.github.io/hearth/
   ```

   （本仓库名为 `hearth`，则站点地址为 `https://flairamos.github.io/hearth/`）

## 首次部署检查清单

- [ ] `docs/.nojekyll` 存在（已生成，跳过 Jekyll 处理，加快发布）
- [ ] Pages Source 为 `Deploy from a branch` + `main` + `/docs`
- [ ] 打开站点后导航六个页面均可跳转（全部为相对链接，适配 `/hearth/` 子路径）
- [ ] 若样式未生效：确认访问的是 `…/hearth/style.css` 而不是根路径

## 目录结构

```
docs/
├── .nojekyll        # 禁用 Jekyll（纯静态，发布更快）
├── index.html       # 首页（简介 / 特性 / 快速开始 / 性能）
├── install.html     # 安装（Windows 优先 / 启动方式 / 迁移）
├── usage.html       # TUI 使用指南（快捷键 / 斜杠命令 / @引用 / agent 模式）
├── agent.html       # hearth-agent（无头用法 / 工具集 / access 三档 / --json）
├── config.html      # 配置参考（全字段 / 环境变量 / 数据位置）
├── architecture.html# 架构（分层 / 设计决策 / 消息生命周期 / 目录速查）
├── style.css        # 主题样式（炉火暗色，零外部依赖）
└── DEPLOY.md        # 本文件
```

## 注意事项

- **相对路径**：全站使用相对链接（`style.css`、`install.html`），因此
  无论是项目页（`/hearth/`）还是用户主页（`/`）都可用，无需配置 baseurl。
- **更新发布**：直接修改 `docs/` 下文件并 push 到 `main` 即自动重新发布。
- **自定义域名**：Settings → Pages → Custom domain，无需改动本目录。
- **改用 gh-pages 分支**：如果不希望文档出现在 main 的 `/docs`，可
  `git subtree push --prefix docs origin gh-pages` 发布到独立分支，
  Pages Source 改为 `gh-pages` 分支 `/ (root)`。
