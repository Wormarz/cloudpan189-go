# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> 注意：本项目 README 声明已不再维护。所有命令帮助、注释、错误提示均为中文，修改代码时保持这一风格。
>
> 维护方式：本仓库是 fork 出来的分支，独立维护（`Wormarz/cloudpan189-go`），不再向上游合并。
> 项目细节、进度记录、后续计划统一在 Notion 上维护和跟踪（页面「cloudpan189-go」：
> https://app.notion.com/p/3d224b48f55980938c90d8ea7c996e5f ），
> **不要在仓库内的文件（docs/ 等）里记录项目进度**。需要查阅或更新项目进度时，用 notion MCP 访问该页面。

## 构建与测试

- Go 1.20 模块，模块路径 `github.com/tickstep/cloudpan189-go`。三个外部 tickstep 依赖是独立模块（`cloudpan189-api` 为官方 API 绑定，`library-go` 为公共工具库）；go.mod 底部有注释掉的 `replace` 指令用于本地联调它们。
- 单二进制，入口在仓库根目录的 `main.go`：`go build` 即可得到 `cloudpan189-go`。
- 版本号通过 `-ldflags "-X main.Version=<version>"` 注入（默认 v0.1.3），不要硬编码新版本号到代码。
- 全平台交叉编译：`./build.sh <version>`（输出到 `out/`，含 Android/iOS/龙芯 loong64）。Windows 用 `win_build.bat`。
- Windows 图标/应用信息由 `versioninfo.json` + `bin/*/goversioninfo` 生成 `.syso`，改 `versioninfo.json` 后按 `docs/complie_project.md` 重新生成，否则提交的 `.syso` 过期。
- 测试极少，仅 `internal/taskframework`、`internal/waitgroup`、`internal/file/uploader`、`internal/config` 四个包有 `_test.go`。运行：`go test ./...`；单测：`go test ./internal/taskframework/`。无 lint/CI 配置。

## 架构总览

仿 Linux shell 的天翼云盘命令行客户端，整体分层：

1. **入口 `main.go`** — 用 urfave/cli v1 定义 App，在 `app.Commands` 中注册全部命令（每行一个 `command.CmdXxx()`）。无参数启动进入交互式 REPL（基于 `github.com/peterh/liner`，`cmder/cmdliner` 封装），支持 tab 补全（补全走 `config.CacheFilesDirectoriesList` 缓存）。启动时先 `checkLoginExpiredAndRelogin`。**新增命令 = 在 `internal/command/` 加一个返回 `cli.Command` 的 `CmdXxx()` + 在 main.go 注册。**
2. **`cmder/`** — CLI 胶水。`cmder_helper.go` 全局函数 `ReloadConfigFunc`/`SaveConfigFunc`（多数命令的 `Before`/`After` 钩子）、`DoLoginHelperWithQrCode`（交互登录，APP 密码失败自动引导扫码登录，唯一登录入口）、`TryLogin`（会话失效后的自动重登，同样带扫码兜底）。登录态恢复逻辑：`config.ActiveUser()` 在 `panClient` 缺失时用已保存 token 调 `SetupUserByCookie` 校验恢复，会话无效才返回 nil。`cmdliner/`（REPL 行编辑与 shell 风格参数解析）、`cmdtable/`（表格渲染，包一层 tablewriter）、`cmdutil/`（工作目录、地址、转义 escaper、jsonhelper）。
3. **`internal/command/`** — 每个文件一个命令族：定义 `CmdXxx() cli.Command` + 同包内导出的 `RunXxx()` 业务函数（如 `RunDownload`、`RunUpload`、`RunRapidUpload`、`RunCopy`/`RunMove`）。共享入口：`GetActivePanClient()`（= 当前登录用户的 `PanClient()`）、`GetActiveUser()`、`parseFamilyId(c)`（个人云=0，家庭云>0）。
4. **`internal/config/`** — 全局单例 `config.Config`（*PanConfig）。配置文件 JSON：`cloud189_config.json`，路径由 `CLOUD189_CONFIG_DIR` 环境变量或 `library/homedir` 默认目录决定。存用户列表、登录令牌、并发数、缓存大小、速率限制、保存目录、代理。`PanUser` 持每账号状态（个人云+家庭云工作目录、令牌、`PanClient()`、目录缓存，10 分钟过期）；密码用机器唯一 ID 派生密钥做 AES 加密（`EncryptString`/`DecryptString`，机器 ID 变化会导致旧密文不可解密，解密失败时按设计返回原文）。
5. **`internal/taskframework/`** — 通用并发任务执行器。`TaskExecutor`（lane deque 队列 + 限量并行 goroutine，配 `internal/waitgroup`），`TaskUnit` 接口约定 `Run/OnRetry/OnSuccess/OnFailed/OnComplete/RetryWait` 生命周期与重试。
6. **`internal/functions/pandownload` / `panupload`** — 上传/下载的任务单元层，实现 `TaskUnit` 接口（`DownloadTaskUnit`、`UploadTaskUnit`）并调度底层传输引擎。
7. **`internal/file/downloader/` / `uploader/`** — 底层 HTTP 分片传输引擎。
   - downloader：多线程 Range 分片写入 `io.WriterAt`，边写边算 MD5 校验（`internal/localfile`），断点续传用 `InstanceState` 持久化，进度监控 `monitor.go`。下载中的文件后缀 `.cloudpan189-downloading`。
   - uploader：`MultiUpload` 接口 + 分块上传。**小文件（≤200MiB）走 PC 接口（`api.cloud.189.cn` 的 `createUploadFile` 流程）整文件 PUT**（`CmdUpload` 里 `Parallel=1, NoSplitFile=true`），该接口单请求上限恰好 200MiB（超出 413）；**>200MiB 自动改走 `internal/functions/panupload/web_upload.go` 的 web 分片上传**（`upload.cloud.189.cn`：initMultiUpload → getMultiUploadUrls 预签名分片 → commitMultiUploadFile，params 经 AES-128-ECB 加密 + HMAC-SHA1 签名）。上传中断现场存 `cloud189_uploading.json`。
8. **`internal/localfile/`** — 本地文件抽象，`checksum_write.go` 支持边写边算 md5/crc32（秒传与下载校验的基础）。
9. **`internal/panupdate/`** — 通过 GitHub Releases API 检查/下载更新的自更新，v0.1.3 起默认不自动核验。
10. **`library/`** — 仓库内通用小库：`crypto`（`tool enc/dec` 命令的文件加解密）、`homedir`、`requester/transfer`（下载 Range 列表、实例状态、状态结构）。

秒传（rapid upload）与导入/导出：`import`/`export` 走文件元数据（md5+size+path），升级存储用 `internal/functions/panupload` 里的 bolt 数据库（`sync_database_bolt.go`，`SyncDb` 接口）做断点/重复检测。

## 用户侧要点

- 环境变量：`CLOUD189_CONFIG_DIR` 指定配置目录；`CLOUD189_VERBOSE=1` 开启调试日志（`logger.IsVerbose`，README 有抓 log 的步骤）。
- 命令手册在 `docs/manual.md`，命令帮助文本直接写在各 `cli.Command.Description`（中文），改命令行为时同步更新。
- 交互模式与命令行参数模式跑同一套命令；`loglist/su` 管理多账号，`family` 切换个人云/家庭云，`xcp` 在两者间转存。
- `share set/cancel/list`、`sign`（签到）、`quota`、`tool getip/enc/dec` 等为天翼云特有的 shell 命令集合。