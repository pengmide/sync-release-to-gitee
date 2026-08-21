# release2gitee Go 重构实施计划

> 状态：实施前方案
>
> Rust 1.2.1 是行为基准；新的 Go 实现位于本仓库，旧仓库
> `/Users/pengmd/c/release2gitee` 只用于核验，不做渐进式改写。

## 1. 目标、依据与边界

本项目将把 GitHub Release **单向**同步到 Gitee Release。重构的目标不是
逐行翻译 Rust，而是以 Rust 1.2.1 的正常同步结果为兼容基线，同时有意修复
本地临时文件残留、上传后假成功、凭据暴露和不可测试性。

本计划的事实依据按优先级如下：

1. Rust 1.2.1 源码，特别是同步入口、规划与附件逻辑
   [src/lib.rs](/Users/pengmd/c/release2gitee/src/lib.rs:20)、CLI 模型
   [src/model.rs](/Users/pengmd/c/release2gitee/src/model.rs:7) 和 HTTP 层
   [src/http.rs](/Users/pengmd/c/release2gitee/src/http.rs:16)。
2. 重构前调研交接文档
   [handoff-go-rewrite-analysis.md](/Users/pengmd/tmp/handoffs/release2gitee/master/handoff-go-rewrite-analysis.md:1)。
3. 本文；若本文与旧 README 的描述冲突，以源码和本文为准。

### 1.1 第一版必须保持的行为

- GitHub 是唯一数据源，Gitee 只作为写入目标。
- `tag_name` 是 Release 的唯一匹配键；Gitee 存在同 tag 时无条件跳过，**不**更新
  名称、正文、预发布标记或附件，也不补传缺失附件。
- GitHub 只取 `page=1`，`per_page` 为 `github_latest_release_count`；Gitee 只取
  `page=1`，`per_page=100`。第一版不引入完整分页，避免悄然扩大白名单和清理范围。
- 计算附件保留范围时，先放入 Gitee Release，再追加 GitHub Release；按兼容版本规则
  稳定地从新到旧排序；按 `tag_name` 去重并保留第一次出现的记录；取前 N 个 tag。
  这里的“附件白名单”是 **Release tag 白名单**：入选 tag 的新 GitHub Release 会上传
  它的全部 GitHub assets，不是挑选某几个附件。
- 不在白名单且 Gitee `assets.len() > 2` 的 Release 仍采用“删除整个 Release 后重建”
  的方式清理额外附件。`assets.len() <= 2` 一律不清理；这是对 Gitee 自动源码附件数的
  既有业务假设，而不是通用的附件差异修复。
- 清理完成后必须重新读取 Gitee Releases，因为重建会产生新的 Release ID。
- GitHub Release 的实际执行顺序保持为 GitHub Release ID 升序。旧实现把列表先按 ID
  降序保存、再反向遍历；它通常对应“旧到新”，但实现契约是 ID 顺序，不能改成按版本号
  执行。
- 新建的非白名单 Release 只创建元数据；白名单内且有附件的 Release 先下载全部附件，
  再创建 Gitee Release，最后上传全部附件。
- GitHub Release 的 `body` 为 `null` 或空字符串时使用 tag；仅空白字符不是空字符串。
- `body` 与名为 `latest.json` 的附件继续执行精确字符串替换：
  `https://github.com/{github_owner}/{github_repo}` →
  `https://gitee.com/{gitee_owner}/{gitee_repo}`。不解析 Markdown 或 JSON，也不扩大
  替换范围。
- 创建 Release 使用 `--gitee-branch` 指定的分支；未指定时先查询 Gitee 默认分支。
- 正常同步保持顺序执行，不并行下载、创建或上传。

### 1.2 有意修复的行为

以下是明确授权的兼容性外改进；它们不改变一次成功同步的目标 Release 集合。

- 改为每次运行独立的 staging 目录，成功和失败路径均 best-effort 清理，不复用 Rust
  的长期临时缓存。
- 下载使用 `.part → 校验 → 原子 rename`；附件名不参与本地路径拼接。
- 缺失本地文件、下载失败、创建失败或附件上传失败必须令该 Release 和整次运行失败，不能
  像旧实现那样仅记录日志后返回成功。
- 对“上传请求结果未知”先查询远端状态；确认不完整时只回滚本次刚创建的 Release，并在
  回滚失败时返回带 tag、Release ID 和已完成附件信息的“远端部分完成”错误。
- 日志、错误和 dry-run 输出不得包含 token、Authorization header 或 token 前缀。
- multipart 的 `file` 字段继续保持，但 filename 只使用原始附件名，绝不发送本地绝对路径。
- 增加 `--dry-run`，在所有写操作前输出确定性计划并退出。
- HTTP 客户端、时钟/等待策略和 API base URL 可注入，以便本地契约测试；生产默认地址仍是
  GitHub/Gitee 的现有 API。

### 1.3 第一版明确不做

- 自动更新已存在的 Gitee Release，或补齐历史缺失附件。
- 全量分页、私有 GitHub 附件的认证下载、断点续传、常驻下载缓存。
- 并行上传/创建 Release，或把 `delete_all_releases.sh` 变成 Go 子命令。
- 以严格 SemVer 替代 Rust 的宽松版本比较规则。
- 未经单独设计就对清理后的 Release 自动恢复“原来的 target_commitish”。旧实现重建时
  已固定使用当前选择的目标分支。

## 2. 基线同步契约

### 2.1 术语

| 符号 | 含义 |
| --- | --- |
| `G` | 本次从 GitHub 第一页读取到的 Release 列表 |
| `T0` | 清理前从 Gitee 第一页读取到的 Release 列表 |
| `T1` | 清理完成后重新读取到的 Gitee Release 列表 |
| `N` | `gitee_retain_release_attach_files_count`，默认 3 |
| `R` | 允许保留额外附件的 tag 集合 |

### 2.2 确定性规划算法

```text
读取 G、T0 和 target_commitish
  ↓
候选列表 = T0（保持原始顺序） + G（保持原始顺序）
  ↓
按兼容比较器稳定地按版本从新到旧排序
比较失败 = 相等；因此稳定性决定次序
  ↓
按 tag_name 去重，保留第一次出现的记录
  ↓
R = 前 N 个记录的 tag_name
  ↓
对 T0：tag ∉ R 且 assets.len() > 2 的记录计划“删除并重建”
  ↓
执行清理，重新读取 T1
  ↓
按 GitHub Release ID 升序逐个处理
```

兼容比较器必须隔离在 `internal/planner`，并以表格测试锁定以下事实：

- `v1.2.3` 与 `1.2.3` 等价；不能直接套用严格 SemVer 库。
- 排序比较失败时返回相等，不报错、不把不可比较 tag 排到某一端。
- 排序必须稳定。两个记录在比较上相等、且 tag 相同的时候，`T0` 位于 `G` 之前，所以
  去重后保留 Gitee 记录。
- 白名单排序和同步执行顺序是两件事：前者按兼容版本排序，后者按 GitHub Release ID 升序。

### 2.3 单个 Release 的动作表

| 条件 | 动作 |
| --- | --- |
| `T1` 中已有完全相同的 `tag_name` | 跳过；不比较也不修复任何字段或附件 |
| 不存在同 tag，且 tag 不在 `R` | 创建仅含元数据的 Gitee Release |
| 不存在同 tag，tag 在 `R`，但 GitHub assets 为空 | 创建仅含元数据的 Gitee Release |
| 不存在同 tag，tag 在 `R`，且 GitHub assets 非空 | 下载全部 assets → 创建 Release → 上传全部 assets |

Gitee 的清理和 GitHub 的同步是两个独立步骤：清理只面向 `T0` 中非白名单且附件数大于 2
的旧对象；同步时仍只依据 `T1` 中有没有同 tag。

## 3. Go 项目结构与职责

```text
sync-release-to-gitee/
├── go.mod
├── go.sum
├── cmd/
│   └── release2gitee/
│       └── main.go                 # 进程入口、退出码
├── internal/
│   ├── config/                     # CLI、环境变量、校验、脱敏摘要
│   ├── domain/                     # 平台无关的 Release、Asset、Plan、Result
│   ├── github/                     # GitHub DTO、列表读取、附件下载
│   ├── gitee/                      # Gitee DTO、列表/分支/创建/删除/上传
│   ├── httpx/                      # Client、超时、重试、错误脱敏、限额
│   ├── planner/                    # 纯函数：排序、去重、白名单、执行计划
│   ├── staging/                    # run/release 目录、manifest、原子文件、清理
│   ├── transform/                  # 精确 URL 替换
│   ├── progress/                   # 可关闭的下载/上传进度显示
│   └── syncer/                     # 预检、编排、补偿、运行摘要
├── testdata/
│   ├── github/
│   ├── gitee/
│   └── planner/
├── docs/
│   ├── plan.md
│   ├── behavior-contract.md
│   ├── operations.md
│   └── recovery.md
├── examples/
│   └── sync-to-gitee.sh
└── .github/workflows/
```

设计约束：

- 平台 DTO 和 `domain` 模型分开；DTO 不跨越 client 边界。
- `planner` 不访问网络、不访问文件系统、不可变输入 → 确定性输出。
- `syncer` 不自行拼 URL 或解析平台 JSON，只编排 `planner`、clients 与 staging。
- 一切网络、文件和等待都接受 `context.Context`；取消会阻止后续操作并走清理路径。
- 生产 client 不使用全局可变状态；测试通过依赖注入替换 HTTP server、时钟和 sleeper。
- 二进制名继续为 `release2gitee`；仓库名变化不影响命令调用。

## 4. CLI、环境变量与日志契约

### 4.1 用户配置

| 配置 | CLI | 默认/是否必填 | 环境变量兼容 |
| --- | --- | --- | --- |
| GitHub owner | `--github-owner` | 必填 | `GITHUB_OWNER`；旧 README 小写 `github_owner` 也接受 |
| GitHub repo | `--github-repo` | 必填 | `GITHUB_REPO`；`github_repo` 也接受 |
| GitHub token | `--github-token` | 可选 | `GITHUB_TOKEN`；`github_token` 也接受 |
| Gitee owner | `--gitee-owner` | 必填 | `GITEE_OWNER`；`gitee_owner` 也接受 |
| Gitee repo | `--gitee-repo` | 必填 | `GITEE_REPO`；`gitee_repo` 也接受 |
| Gitee token | `--gitee-token` | 必填 | `GITEE_TOKEN`；`gitee_token` 也接受 |
| GitHub 查询数量 | `--github-latest-release-count` | 5 | `release2gitee__github_latest_release_count` |
| 保留附件的 tag 数 | `--gitee-retain-release-attach-files-count` | 3 | `release2gitee__gitee_retain_release_attach_files_count` |
| 替换正文 URL | `--release-body-url-replace` | true | `release2gitee__release_body_url_replace` |
| 替换 `latest.json` URL | `--latest-json-url-replace` | true | `release2gitee__latest_json_url_replace` |
| 创建目标分支 | `--gitee-branch` | 可选 | `release2gitee__gitee_branch` |
| 详细度 | `-v` / `--verbose`、`-q` / `--quiet` | Info | 无 |
| 版本 | `--version` | — | — |
| 演练 | `--dry-run` | false（新增） | 无 |

基础字段的 Rust 源码仅以 `#[clap(long, env)]` 声明环境变量，旧 README 又展示了小写拼写。
Go 版将上表的大写形式定为标准名，同时接收小写兼容别名，避免把这类文档/运行时差异变成迁移
中断。优先级为：

```text
显式 CLI 参数
  > 大写标准环境变量
  > 小写兼容别名
  > 默认值（只有具有默认值的字段）
```

两个大小写环境变量同时存在且值不同，选择大写值并发出**不含值**的配置冲突警告。显式命名的
`release2gitee__...` 变量按 Rust 的拼写原样支持；其优先级位于 CLI 与默认值之间。

布尔参数必须允许显式传入 `true` 或 `false`；这是对默认值为 `true` 时易用性的补强。所有
兼容路径都要由子进程级 CLI 测试锁定 `--help`、`--version`、缺失必填参数和冲突环境变量的
退出码及 stderr 结构。

### 4.2 日志与秘密

- 配置摘要可打印 owner、repo、计数、分支和开关；token 一律显示为 `<redacted>`，不得保留
  任何前缀。Rust 版会显示 token 前 8 个字符，这个行为不迁移。
- 禁止记录 Authorization、Cookie、请求完整 payload、完整响应体和包含 token 的 URL。
- HTTP 错误可记录 status、请求方法、脱敏 URL 和长度受限/脱敏后的错误摘要；不把服务端无限
  长 body 读入内存或日志。
- `--dry-run` 只输出 tag、操作类型、旧/新 Release ID（若已知）、附件原名和原因；不显示
  token、本地 staging 绝对路径或 Authorization 信息。

## 5. 执行流程与预检

```text
解析配置、建立安全日志
        ↓
预检：必填值、base URL、目标分支、读取连通性、staging 根目录
        ↓
创建本次运行的 staging 根目录
        ↓
读取 G、T0，生成确定性 Plan
        ↓
dry-run？── 是 → 输出 Plan，清理空 run 目录，退出 0
        │
        否
        ↓
执行 T0 的删除/重建清理
        ↓
重新读取 T1
        ↓
按 GitHub Release ID 升序逐个同步
        ↓
输出摘要；defer 清理 run 目录
```

删除任何旧 Gitee Release 前必须已完成以下预检：目标分支可确定、创建 payload 可构造、Gitee
token 已配置且读取请求成功、对应 Release 的可重建元数据已保存。写权限只能由实际写请求确认；
该清理操作无法做成真正事务，删除成功、重建失败时必须返回明确的恢复信息，而不是把它伪装成
普通重试。

旧版的 1 秒（删除后）和 3 秒（创建后）等待迁移为注入式 `RatePolicy` 的默认值，以便在测试
中不 sleep。第一版仍保留这些默认等待，不根据本次改造擅自调整对 Gitee 的节奏。

## 6. Planner 的数据模型与输出

`planner.Plan` 至少包含：

```text
RetainedAssetTags []Tag
Cleanup            []RecreateRelease
Sync               []SyncRelease          # GitHub Release ID 升序
SkippedExisting    []Tag
CreateMetadataOnly []Tag
CreateAndUpload    []Tag
```

其中 `Cleanup` 必须包含删除前的 Release ID 与可重建的 `tag_name`、`name`、`body`、
`prerelease`；`SyncRelease` 必须说明是否在白名单内、是否有 assets、预期动作与判定原因。
Plan 不持有 token，不生成 staging 本地路径。

`--dry-run` 的输出顺序和真实执行顺序一致，且包含：输入页大小、白名单 tag、将清理的 tag、
每个 GitHub Release 的动作与同 tag 跳过原因。对同一组输入重复运行必须字节级稳定，方便
审阅破坏性清理。

## 7. Staging、附件与转换

### 7.1 目录和命名

Rust 版使用 `TMPDIR/{github_repo}/{tag_name}/{asset_name}` 并把文件保留在磁盘。Go 版使用：

```text
$TMPDIR/release2gitee/run-<timestamp>-<pid>-<random>/
└── release-<opaque-id>/
    ├── .release2gitee-staging-marker
    ├── manifest.json
    ├── asset-001.part
    └── asset-001
```

- run 目录以受限权限创建；`opaque-id` 仅由受控随机值或哈希组成。
- tag 和原始附件名只写入 manifest，不直接作为本地路径片段。
- 只接受单一文件名的附件；拒绝空名、绝对路径、`..`、路径分隔符、NUL、平台保留名、过长名称
  和同一 Release 内的大小写碰撞。拒绝时不创建远端 Release。
- multipart filename 使用 manifest 中保存的原始附件名；本地文件仍可叫 `asset-001`。

### 7.2 原子下载与 `latest.json`

每个白名单 Release 先完成全部附件准备，再写远端对象：

```text
创建 release staging 目录与 manifest
        ↓
校验全部附件名称与资源限额
        ↓
逐个下载到同目录随机 .part 文件
        ↓
校验 HTTP 成功状态和 GitHub 原始 size（若 API 提供）
        ↓
asset 名为 latest.json 且开关开启？
  ├─ 否：原子 rename 为完成文件
  └─ 是：读取 UTF-8、精确替换 URL、原子写入完成文件
        ↓
全部文件准备成功后才创建 Gitee Release
```

`latest.json` 的 size 校验发生在源文件转换之前；转换后的大小不必等于 GitHub asset 的 size。
开关开启且 `latest.json` 不是 UTF-8 时失败，保持 Rust 的“不能安全转换即不上传”语义。任一
下载或转换失败都删除 `.part` 和 release staging，并且不创建 Gitee Release。

### 7.3 清理策略

- 一个 Release 的所有上传确认成功后，立即删除该 Release staging 目录。
- 下载、转换、创建或上传失败时也 best-effort 删除；每个 run 以 `defer` 兜底删除。
- 清理失败只在主要同步已成功时降为 warning，输出受控的残留路径；它不能把成功同步改为失败。
- 启动时只清理程序专属根目录内、带 marker、超过安全年龄阈值的旧 run 目录；绝不遍历
  `TMPDIR` 的其他位置，也不删除无 marker 的目录。
- 第一版没有缓存和断点续传。进程遭 `SIGKILL` 或机器断电时无法依赖 `defer`，由下一次启动的
  过期 run 清理兜底。

## 8. HTTP、超时与失败补偿

### 8.1 HTTP 规则

- GitHub/Gitee 元数据读取、下载、创建、删除、上传都经过 `httpx`；生产默认 `User-Agent` 为
  `release2gitee/<version>`。
- 元数据请求与大文件传输使用不同的 connect、请求和 idle 超时；所有响应与下载均有可配置的
  安全上限。
- GET/list/download 可对传输错误、429 和可重试 5xx 做有限指数退避加 jitter，并尊重
  `Retry-After`。重试次数、最大等待和总 deadline 都有上限。
- POST/DELETE/upload 不做盲目重放。连接中断或超时时，先查询远端可观察状态再决定是否继续、
  补偿或报部分完成。
- Gitee API 认证保持 `Authorization: token <token>`；GitHub API 请求按现有行为可带可选 token。
  第一版不改变 GitHub asset 下载为私有仓库提供认证的范围。
- 创建 Release 使用 JSON；上传严格使用 multipart 字段 `file`。

### 8.2 创建、上传和补偿

| 失败点 | 必须结果 |
| --- | --- |
| 附件下载或转换失败 | 不创建 Gitee Release；清理 staging；返回失败 |
| 创建 Gitee Release 失败 | 清理 staging；返回失败 |
| 第 N 个附件上传明确失败 | 查询本次创建的 Release；若附件已存在则继续，否则删除本次创建的 Release 并返回失败 |
| 上传结果未知（超时/断链） | 先查询该 Release 与附件；确认存在则继续，确认缺失则走回滚 |
| 补偿删除失败 | 返回“远端部分完成”错误，包含 tag、Release ID、已确认上传的附件和恢复建议；不含 token |
| 运行开始前已有同 tag | 一律跳过，不删除、不回滚、不修复 |

附件存在性优先按原始文件名确认；若 Gitee API 返回可用大小等元数据，也必须纳入检查。远端删除
仅限本次运行创建的 ID，禁止根据 tag 模糊删除，以免误删并发创建的对象。

## 9. 测试与验收体系

### 9.1 单元和表格测试

- 配置：默认值、CLI 覆盖环境变量、大小写兼容别名、冲突警告、布尔 false、必填校验。
- 秘密：错误、日志和 Plan 序列化中均不存在 token 或 token 前缀。
- Planner：`v1.2.3`/`1.2.3`，不可比较 tag，稳定排序，Gitee 先于 GitHub 的同 tag，去重，
  前 N tag，`assets.len()` 分别为 0/1/2/3，白名单与 ID 执行顺序独立。
- 变换：空/非空 body、开关、严格 URL 字符串替换、只转换名为 `latest.json` 的 asset。
- Staging：跨平台非法名、路径穿越、Unicode、大小写碰撞、原子 rename、成功/失败清理和过期
  marker 清理边界。

### 9.2 HTTP 契约测试

使用本地 `httptest` server 验证：

- GitHub/Gitee 的 URL、query、认证头、User-Agent 和 JSON payload。
- GitHub/Gitee 第一页限制，及 Gitee 默认分支查询。
- 非 2xx、受限错误 body、取消、超时、`Retry-After` 和重试边界。
- 下载中断、size 不匹配、`.part` 不被误当成完整文件。
- multipart 的字段名为 `file`，filename 为原始附件名而非本地路径。

### 9.3 编排测试

- 同 tag 跳过，即使远端 body 或附件不完整。
- 非白名单只建元数据；白名单内上传 GitHub 的全部 assets。
- 下载失败不创建 Release；创建失败不残留 staging。
- 上传失败/结果未知触发查询、补偿与部分完成报告。
- 清理中的删除 → 等待 → 重建 → 刷新顺序；后续上传必须使用刷新后的新 Release ID。
- 所有成功和受控失败路径均不残留已完成 Release 的本地附件。
- `--dry-run` 不发送 POST、DELETE 或 upload，并输出与真实计划一致的动作。

### 9.4 CI 与发布验证

- 每次变更执行无修改式的 `gofmt` 校验、`go vet ./...`、`go test ./...` 和 Linux `go test -race ./...`。
- GitHub Actions 在 Linux、macOS、Windows 原生 runner 上构建，并在各自平台执行 `--help`、
  `--version` 和 mock end-to-end smoke test。
- 发布前交叉/原生构建至少覆盖 Linux amd64、Windows amd64、macOS amd64 与 arm64；若发布
  universal macOS 包，单独实现并测试 lipo/签名流程，不能照抄 Rust 的 target 名称。
- 提交 `go.sum`；发布 workflow 删除 Rust toolchain 与 crates.io 发布阶段，改为上传 Go 二进制
  制品。Actions 使用经审阅的固定版本/commit。
- 真实 Gitee 写入测试只能在手动触发或受保护的 workflow 中执行，凭据只来自 CI Secret。

## 10. 实施阶段与退出条件

| 阶段 | 交付物 | 退出条件 |
| --- | --- | --- |
| P0 基线与安全 | 行为契约、秘密扫描、CI secret 说明 | 不把未验证的 README 掩码当作泄露；任何确认泄露的 token 已撤销/轮换；日志策略通过测试 |
| P1 骨架与配置 | Go module、CLI、config、domain、结构化日志 | 参数、环境变量、退出码和脱敏测试通过 |
| P2 只读 clients | GitHub/Gitee DTO、列表、默认分支、`httpx` | 本地 HTTP 契约测试覆盖所有读取接口 |
| P3 Planner 与演练 | 兼容比较器、Plan、`--dry-run` | 全部规划表格测试通过，dry-run 稳定且无写请求 |
| P4 Staging 与转换 | 安全命名、原子下载、`latest.json`、清理 | 成功/错误/异常恢复测试无非预期残留 |
| P5 写操作与补偿 | 创建、删除、上传、远端状态确认 | mock 编排测试覆盖失败、回滚和部分完成 |
| P6 完整同步 | 清理、刷新、ID 顺序、运行摘要 | 本文第 2 节和第 9 节的行为验收全部通过 |
| P7 发布迁移 | README、运维/恢复文档、跨平台 workflow 和制品 | 三个平台产物可执行，CI 不再依赖 Rust/crates.io |
| P8 灰度切换 | 测试仓库、非关键仓库、生产切换记录 | 多次重复运行结果稳定；每次破坏性清理均先经 dry-run 审阅 |

## 11. 上线、回退与运维

- 发布前在测试 Gitee 仓库执行至少一次 dry-run 和一次真实同步；记录输入页大小、白名单、清理
  tag 与产物校验值。
- 灰度期间保留 Rust 1.2.1 二进制作为只读行为比对工具，但同一目标仓库的同一轮同步只能由一个
  实现执行，避免竞争创建/删除。
- 新版失败时停止本轮后续写入，保留不含凭据的运行摘要；若出现“远端部分完成”，按
  `docs/recovery.md` 基于 tag 与 Release ID 人工确认后处理。
- 回退到 Rust 只适用于尚未执行新版清理/创建写操作的情况。若新版已删除重建或部分上传，先以
  Gitee 当前状态为事实源完成恢复，不能盲目再次同步。
- 不迁移旧仓库中危险的“删除全部 Release”脚本能力；若未来需要，必须另行设计权限范围、二次
  确认、目标回显和审计日志。

## 12. 完成定义

本重构完成不以“Go 能编译”为准，而同时满足：

1. 正常输入下，本文第 2 节的 Release 创建/跳过/清理结果与 Rust 1.2.1 基线一致。
2. 附件白名单、稳定排序、同 tag 跳过、第一页范围和 GitHub Release ID 执行顺序都有自动化
   回归测试。
3. 每个本次下载的附件在成功或可控失败后都无意外本地残留；异常残留只可能在带 marker 的
   程序专属目录，并可由后续运行安全清理。
4. 上传不完整、补偿失败和破坏性清理失败均会返回可操作但不泄露凭据的错误。
5. Linux、macOS、Windows 的发布制品、帮助、版本和 mock smoke test 均已验证；CI 与文档
   不依赖 Rust 或 crates.io。
