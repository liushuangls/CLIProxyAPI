# self 分支：认证文件用量历史与 GHCR 镜像

`main` 用于同步 `router-for-me/CLIProxyAPI`，个人代码、构建和部署配置只放在 `self`。
管理前端同样保留 `main` / `self`：
[liushuangls/Cli-Proxy-API-Management-Center](https://github.com/liushuangls/Cli-Proxy-API-Management-Center/tree/self)。
本仓库的 `management-ui` 子模块固定前端提交，镜像不会在构建时追随浮动的前端分支。

## 功能与统计口径

认证文件卡片底部的「用量统计」打开右侧抽屉，提供最近 24 小时、7 天、30 天的：

- 按上游请求模型汇总的调用次数、成功/失败、成功率与 token 明细。
- 输入、输出、缓存读取、缓存写入、推理、未分类和总 token。
- 分页请求记录：开始时间、模型、结果、HTTP 错误码、耗时和 token。

成功率按该认证文件实际执行的调用尝试计算：成功次数 /（成功次数 + 失败次数）。
重试分别记录；流式中断按用量事件中的失败状态记录。附加模型计费记录（例如图片工具）
累计 token，但不增加调用次数。没有调用时成功率显示 `—`。

缓存读取和写入已包含在输入总量中，推理已包含在输出总量中，不重复相加。
总量 = 输入 + 输出 + 未分类。只使用上游报告的用量，不估算缺失的 token；
无法可靠分类的记录显示提示。分组使用上游请求模型，返回模型不同会在明细中注明。

统计通过稳定的 `auth_index` 归属认证文件，不根据邮箱或客户端 API Key 混合账户。
同一文件重新认证保留历史；改变文件身份或文件名可能产生新的 `auth_index`。
卡片原有成功/失败计数是运行时统计，与抽屉的持久化时间范围口径不同。

## 保存与配置

历史默认开启，保留 30 天，独立于原有短期用量导出队列。
无需开启包含请求/响应正文的请求日志，也不要求开启 `usage-statistics-enabled`。

```yaml
observability:
  usage:
    history:
      disabled: false
      path: "" # 默认 <oauth.auth-dir>/usage-history.db
      retention-days: 30 # 0 使用默认 30；有效范围 1–365
```

这些设置变更需要重启。默认使用现有认证目录的数据卷，容器重建后历史仍在；
自定义 `path` 时，需要为其所在目录配置持久化挂载。数据库文件权限为 `0600`。
其中只保存统计所需元数据，不保存 API Key、OAuth token、提示词、响应正文或错误正文。

过期记录在启动和每小时清理，查询也即时应用保留期限。扩大保留期限不会恢复已清理数据。
bbolt 会复用释放的页，数据库文件通常不会立即缩小。正常退出会等待用量队列写完。
关闭页面不影响采集；启用前的历史无法补回。存储无法打开时启动失败，写入失败或读到损坏数据时
查询返回明确错误，不用空统计掩盖问题。

## 镜像与部署

推送后端 `self` 会触发 `.github/workflows/self-image.yml`。验证后端和固定版本前端后，
在原生 AMD64/ARM64 runner 上构建并检查容器，全部通过才发布多架构标签：

- `ghcr.io/liushuangls/cliproxyapi:self`：最近一次成功发布。
- `ghcr.io/liushuangls/cliproxyapi:sha-<完整后端提交 SHA>`：用于固定版本和回滚。

镜像携带定制 `management.html`，并设置 `MANAGEMENT_PANEL_BUNDLED=true`，防止后台更新
覆盖内置前端。该模式下缺失面板会报错，不会下载官方页面替代。升级前端需要构建新镜像。
`Dockerfile` 和上游按标签发布的 Docker Hub 工作流保持不变，个人镜像使用 `Dockerfile.self`。

使用仓库已有 Compose 时，在部署目录的 `.env` 设置：

```dotenv
CLI_PROXY_IMAGE=ghcr.io/liushuangls/cliproxyapi:self
```

然后执行：

```bash
docker compose pull cli-proxy-api
docker compose up -d --no-build cli-proxy-api
docker compose logs --tail=100 cli-proxy-api
```

保留现有配置、认证文件、日志及插件挂载。若另有旧 `static` 目录挂载，需检查是否遮挡镜像里的
`/CLIProxyAPI/static/management.html`。若 GHCR 包为私有，先使用具备 `read:packages`
权限的凭据通过 `docker login ghcr.io` 登录。回滚时将镜像值改为之前的 `sha-...` 标签，再拉取启动。

本地构建（需要 Docker 服务运行）：

```bash
git switch self
git submodule update --init --recursive
docker build -f Dockerfile.self -t cliproxyapi:self .
bash scripts/smoke-self-image.sh cliproxyapi:self
```

## 同步上游与更新前端

在工作区干净时分别对两个仓库操作：

```bash
git fetch upstream
git switch main
git merge --ff-only upstream/main
git push origin main
git switch self
git merge main
# 解决冲突并验证后再推送 self。
```

前端 `self` 验证、提交并推送后，在后端更新固定版本：

```bash
git switch self
git submodule update --init
git -C management-ui fetch origin self
git -C management-ui checkout --detach origin/self
git add management-ui
git commit -m "chore(ui): update bundled management UI"
git push origin self
```

前端单独推送不会自动改变已发布的后端镜像；必须更新子模块指针并推送后端 `self`。
镜像标签和 OCI 元数据均记录后端 SHA，`io.cliproxyapi.management-ui.revision` 记录前端 SHA。

## 多轮试用与缓存率

认证卡片的「试用」打开聊天抽屉，可选择该认证文件支持的文本模型，进行简单多轮聊天。
每次调用固定到当前认证文件，失败时不会换用其他账号，调用也会计入用量统计。
支持停止生成和开始新对话；新对话后可以更换模型。关闭抽屉会停止当前生成，
重新打开同一卡片保留已完成的对话；切换卡片或刷新页面后清空。最多 20 轮，
单条输入 4,000 字符，总上下文 32,000 字符，达到限制时需开始新对话。

用量统计的整体卡片、模型汇总及请求明细新增缓存率：
`缓存读取 token / 输入 token × 100%`。汇总按累计 token 计算，不平均各请求的百分比；
没有输入 token 时显示 `—`。缓存写入不计作缓存命中。
