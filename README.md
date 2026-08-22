# sync-release-to-gitee

sync-release-to-gitee 是一个将 GitHub Releases 单向同步到 Gitee
Releases 的 Go 命令行工具。

GitHub 是唯一数据源；Gitee 中存在相同 tag 的 Release 会被跳过。工具会按照
兼容旧版 Rust 工具的规则计算最近版本的附件保留范围，并在本地使用单次运行的
临时目录处理附件，避免长期残留下载文件。

## 构建

需要 Go 1.26 或更高版本。执行下面的脚本会按当前机器的 CPU 架构构建：

    ./scripts/build-dist.sh

二进制文件生成在：

    dist/sync-release-to-gitee

构建脚本默认注入版本号 dev。发布构建可以指定版本：

    VERSION=v1.0.0 ./scripts/build-dist.sh

## 同步仓库

通过环境变量指定源和目标仓库：

    export GITHUB_OWNER=example
    export GITHUB_REPO=source-repository
    export GITHUB_TOKEN='可选；用于提高 GitHub API 额度'
    export GITEE_OWNER=example
    export GITEE_REPO=mirror-repository
    export GITEE_TOKEN='你的 Gitee Token'

    ./dist/sync-release-to-gitee --dry-run
    ./dist/sync-release-to-gitee

命令行参数的优先级最高；其次为大写环境变量。为兼容旧配置，仍接受小写的
github_owner、github_repo、github_token、gitee_owner、gitee_repo 与
gitee_token。

以下旧版长环境变量名称也继续支持：

- release2gitee__github_latest_release_count，默认值为 5
- release2gitee__gitee_retain_release_attach_files_count，默认值为 3
- release2gitee__release_body_url_replace，默认值为 true
- release2gitee__latest_json_url_replace，默认值为 true
- release2gitee__gitee_branch

## 常用参数

| 参数 | 说明 |
| --- | --- |
| --github-owner、--github-repo | GitHub 源仓库 |
| --github-token | 可选的 GitHub API token |
| --gitee-owner、--gitee-repo、--gitee-token | Gitee 目标仓库及认证 |
| --github-latest-release-count | 从 GitHub 第一页读取的 Release 数量，默认 5 |
| --gitee-retain-release-attach-files-count | 保留额外附件的最新 tag 数量，默认 3 |
| --release-body-url-replace=false | 关闭 Release 正文中的仓库 URL 替换 |
| --latest-json-url-replace=false | 关闭 latest.json 中的仓库 URL 替换 |
| --gitee-branch | 创建 Release 时使用的目标分支；未指定时读取 Gitee 默认分支 |
| --dry-run | 只读取并输出同步计划，不执行创建、删除或上传 |
| -v / --verbose | 输出调试日志 |
| -q / --quiet | 静默模式 |
| --version | 输出版本 |

完整帮助可通过下面的命令查看：

    ./dist/sync-release-to-gitee --help

## 同步规则与安全边界

- GitHub 和 Gitee 都只读取第一页；第一版不会自动分页。
- tag_name 是唯一身份。Gitee 中已有同 tag 时不会更新元数据或补传附件。
- 附件白名单按 tag 计算；白名单内的新 Release 会上传其全部 GitHub 附件。
- 白名单外且附件数大于 2 的旧 Gitee Release，会按既有规则删除后重建为仅保留源码附件的 Release。
- 附件下载完成后才创建 Gitee Release；下载、转换或创建失败时不会上传不完整附件。
- 上传结果未知时会重新查询远端状态；确认不完整时只尝试删除本次刚创建的 Release。
- token、Authorization Header、签名下载 URL 和服务端错误正文不会写入日志。

请不要把 token 写入脚本、命令行参数、日志或 Git 仓库。若曾使用旧脚本中保存的
明文 token，请立即轮换。

## 开发验证

    go vet ./...
    go test ./...
    go test -race ./...

CI 会在 Linux、macOS、Windows 上运行测试与 CLI 冒烟验证，并构建 Linux
amd64、Windows amd64、macOS amd64 和 arm64 制品。推送匹配 vX.Y.Z 的 Git
tag 时，Release workflow 会自动创建 GitHub Release，并上传 Linux amd64、
macOS universal 与 Windows amd64 的压缩制品；不会发布到 crates.io。

## 相关文档

- [实施计划](docs/plan.md)
- [行为契约](docs/behavior-contract.md)
- [运维说明](docs/operations.md)
- [失败恢复说明](docs/recovery.md)
