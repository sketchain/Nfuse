<div align="center">

**语言 / Language：[中文](#chinese) · [English](#english)**

</div>

---

<a id="chinese"></a>

# Nfuse（中文）

Nfuse 使用 nftables 的 **netdev** 钩子对网卡进行 **按端口、双向** 的流量计量，将
用量持久化到 **SQLite**，并通过 **内核级熔断器**（nftables 命名 `quota` → 逐包
`drop`）强制执行每账户配额。一个 **子命令式 CLI** 管理账户、端口、档位和重置——
并提供可交互的 **TUI**，通过 `nfuse tui` 启动。

计量的快路径与熔断器完全运行在内核中；用户态只负责采样、持久化、重置和对账——
因此即使 Go 进程崩溃，熔断器依然生效。

## 安装

【国际用户】使用下面的命令将 Nfuse 安装到你的系统：
```
sudo bash <(curl -fsSL https://github.com/sketchain/Nfuse/raw/refs/heads/main/nfuse.sh)
```
【中国大陆用户】使用下面的命令将 Nfuse 安装到你的系统：
```
sudo bash <(curl -fsSL https://gitd.uh.ink/https://github.com/sketchain/Nfuse/raw/refs/heads/main/nfuse_cn.sh)
```


## 命令结构

`nfuse` 是一个子命令驱动的 CLI。**操作类** 命令是一等公民：每个都是一个精简的 RPC
客户端，通过 socket 驱动守护进程的某一个方法。**生命周期类** 命令负责进程角色。

```
nfuse server   --iface X   运行引擎守护进程（掌管 nft + SQLite）
nfuse tui                  针对运行中的守护进程的交互式终端 UI
nfuse teardown             移除内核规则集并退出
nfuse version              打印版本信息

nfuse list [--json]                账户/端口/用量列表
nfuse add <name> --tier a|b|c …     添加账户
nfuse rm <account> [--cascade]      删除账户
nfuse set-tier <account> --tier …   修改档位 / 限额
nfuse reset <account>               将账户用量清零
nfuse set-usage <account> <bytes>   设置账户用量
nfuse persist                       立即强制进行一次 SQLite 快照
nfuse port add  <account> <start[-end]>
nfuse port edit <port-id> <start[-end]>
nfuse port rm   <port-id>
nfuse port move <port-id> <account>

nfuse token show   <account>        打印账户的 HTTP 查询令牌
nfuse token new    <account>        重新生成账户的查询令牌
nfuse token master [--new]          打印（或重新生成）主令牌
```

不带子命令的 `nfuse` 会打印用法并以非零退出；它不再启动 TUI——请用 `nfuse tui`。

### 服务端 / 客户端角色

同一个二进制以下面两种角色之一运行：

- **`nfuse server`** —— **服务守护进程**。它运行引擎并监听一个 Unix 域 socket。它是
  **唯一** 触碰 nftables 和 SQLite 的进程（采样、持久化、每月重置以及所有变更都走它
  单一的对账路径），因此不存在写竞争。这就是 systemd 保持运行的角色。
- **客户端命令**（`nfuse tui`、`nfuse list`、`nfuse add` 等）—— 它们只连接 socket 并
  将每个动作作为 RPC 发送。它们既不碰内核也不碰数据库。如果守护进程不可达，它们会以
  非零退出并报错，提示你启动服务——**没有内嵌引擎的回退**。

传输是通过 socket 的换行分隔 JSON。变更类 RPC 只返回 `ok`/`err`；成功之后客户端会
通过 `GetState` 重新读取完整状态。所有变更都串行化在引擎的对账锁之后，与采样和重置
循环共享该锁，因此规则集永远不会处于半更新状态。

**单实例。** 启动时守护进程会探测 socket：如果已有一个活着的守护进程应答，它会
**拒绝启动**，而不是删除 socket 并抢占它（两个引擎重建同一张 nft 表会互相破坏）。只有
非正常关闭遗留下来的陈旧 socket 才会被回收。

**客户端重连。** 如果守护进程在 TUI 打开期间被重启（例如 systemd 的
`Restart=on-failure`），一个遇到连接级错误的调用会重新拨号 socket 一次并重放请求，
因此客户端会恢复而不是死掉；如果守护进程仍然宕机，状态栏会显示错误，下一次刷新会重试。

### 冷启动 vs 热重启

启动时守护进程会探测其 nftables 表是否已存在，以决定用量的权威来源：

- **表不存在 = 冷启动**（机器重启了，内核状态为空）→ **以 SQLite 为准**：从数据库中
  为计数器/配额播种来构建该表。
- **表存在 = 热重启**（进程被重新拉起，例如被 `Restart=on-failure`，但机器一直在线）→
  **以内核为准**：采样实时值，将其折算进 SQLite，并据此重建播种，从而使陈旧的数据库
  永远不会覆盖内核仍在跟踪的用量。

## 工作原理

### 计量（内核，网卡上的 netdev）

两条设备绑定的链被挂到受管接口上（`--iface`，例如 `ens5`）：

- **ingress** 钩子 —— 匹配 **目的端口** `P`（外部 → 本地端口 `P`）
- **egress** 钩子 —— 匹配 **源端口** `P`（本地端口 `P` → 外部）

每个受管端口在每个方向上获得 **一个计数器**（`in` = ingress/dport，`out` =
egress/sport），因此 TUI 可以展示按端口、按方向的细分。egress 钩子要求
**Linux ≥ 5.16**；Nfuse 在启动时检查这一点。

#### 端口范围

一个受管条目可以是单个端口（`60006`），也可以是一个 **连续范围**（`60000-60099`），
以 `start-end` 形式录入（`60000 - 60099` 这样两侧的空格是可以容忍的）。一个范围被作为
**整体** 计量和熔断：它渲染为 nftables 原生的范围匹配（`th dport 60000-60099`），并为
整个跨度获得 **恰好一对计数器**（in/out）——范围内部没有按端口的细分。它的字节数像
任何其它端口一样折算进所属账户的共享配额。

范围（以及单个端口）**不得重叠**——跨所有账户、在任何地方都不行。相邻的范围没问题
（`60000-60099` 和 `60100-60199` 可以共存）；任何共享的端口号都会被拒绝。该检查在
引擎对账路径内部原子地强制执行（因此两个并发的添加不可能都塞进一个重叠的范围），并
在每次内核应用之前作为最后一道防线在 `Snapshot.Validate` 中再次检查。一个范围当
`1 ≤ start ≤ end ≤ 65535` 时是良构的。

### 熔断（内核，命名 quota）

一个账户的所有端口引用 **同一个命名 `quota` 对象**，因此内核以 **等权重** 将该账户的
in + out 字节数汇总进单一预算。每条规则布局如下：

```
<match> quota name "acct<id>" drop      # 熔断器：先被求值
<match> counter name "p<portID>_<dir>"  # 明细：只有未被 drop 时才到达
```

在预算之内时 quota 语句求值为 false，`drop` 被跳过，计数器规则运行。一旦超出预算，
quota 产生 `drop`，这会终止规则求值——因此 **计数器位于 drop 之后，一旦熔断器跳闸就
立即停止推进**。丢弃是逐包的，没有优雅处理：新连接和已建立连接被同等对待。不限量账户
**不安装 quota 规则**，永远不会熔断。

这一点经过实证验证：在 50 KB 配额下，一个试图通过环回发送 2.6 MB 的客户端一旦跳闸就
会超时，quota 上限停在 `used 50000`，drop 之后的计数器在限额以下冻结。

### 账户档位

| 档位 | 含义                 | 内核配额     | 用户态重置            |
|------|----------------------|--------------|-----------------------|
| `a`  | 每月 X GiB           | `over X`     | 每月（账单日）        |
| `b`  | 一次性 X GiB         | `over X`     | 从不（永久）          |
| `c`  | 不限量               | 无           | 不适用                |

档位 **a** 和 **b** 在内核中逐字节相同（“超过 X 就丢弃”）。*唯一* 的区别是用户态是否
执行周期性重置。账单锚定日必须为 **1–28**（超出该范围的值会被拒绝而非钳制，因此打错的
日期永远不会悄悄改变账期）。

**切换档位不会清除用量——这是设计使然。** 将账户从 a/b 移到 **c** 只是 *暂停* 计量
（quota 对象被移除）；把它移回来会复活历史 `used_bytes` 并重新播种进内核配额。这是
有意为之，不是 bug：档位 c 是“暂停计费”，而不是“从头开始”。要真正清零用量，请重置
账户（`r`）。

### 持久化与重启回填（SQLite，WAL）

内核 quota/counter 值在重启时 **丢失**。Nfuse 周期性地将每个账户的配额用量和每个
计数器快照到 SQLite（WAL 模式）。启动时它重新加载这些值并 **播种** 新创建的 nft
对象——配额通过 `used <bytes>`，计数器通过 `packets/bytes`——从而使档位 a/b 的强制在
重启后依然准确。这由一个测试验证：持久化 500 MiB、“重启”、并确认重建后的配额报告相同
的 `used`。

写入在自己的 goroutine/ticker 上运行，读取最新的内存采样，因此 **SQLite 写入永远不会
阻塞采样或熔断器**。

### 对象生命周期

每一次变更（添加/删除账户或端口、修改档位）都通过 **全量原子对账** 来应用：取一份
**全新** 的实时采样 → 将其折算进 SQLite → 从单个 `nft -f` 脚本重建整张表
（`add table; delete table; add table`）。这能正确地创建/回收计数器、配额和规则，
无需手工跟踪规则句柄，也不会使规则集处于半更新状态。采样是在 *对账时* 取的（不是复用
周期性采样器的），因此在上一次周期采样与重建之间计量到的流量不会丢失；如果实时表无法
采样，该变更会被拒绝，而不是基于陈旧的账目重建。

一个单调递增的世代计数器防止一次缓慢的周期采样与一次降低用量的变更（reset、每月重置、
set-usage）发生竞态：在这样一次变更完成之前取的采样会被丢弃，而不是把变更前的值写回
覆盖它。

## 为何用 `nft` 而不是 netlink 库

Nfuse 调用外部 `nft(8)` 而非使用 `google/nftables`：

- 整个规则集从一个 `nft -f` 脚本应用，nft 将其作为单一 **原子事务** 运行——免费获得
  无竞态的全量对账。
- `nft -j list` 返回 **结构化 JSON**，使计数器/配额采样成为稳定的解析，而不是手工的
  netlink 属性解码。
- 极小的依赖面，并且与运维人员检视规则集的方式一致。

其代价（每次应用/采样一个进程）无关紧要：应用仅在变更时发生，采样以人类时间尺度运行。

## 布局

```
main.go                 精简垫片：注入构建元数据，交给 internal/cli
internal/cli            子命令分发、操作类命令、--json 输出
internal/model          领域类型 + 不变量（账户→端口→计数器 树）
internal/store          SQLite（WAL）加载/保存 + 重启回填
internal/nft            nftables 管理器：脚本生成 + JSON 采样
internal/engine         采样循环、非阻塞持久化、重置、对账
internal/rpc            Unix-socket JSON RPC：服务端（引擎）+ 客户端（消费者）
internal/httpapi        只读 HTTP 查询端点（curl <host:port>/<token>）
internal/system         内核版本预检（netdev egress ≥ 5.16）
internal/tui            tview UI（驱动本地引擎或 RPC 客户端）
```

## 使用

```sh
go build -o nfuse .

# 服务守护进程（掌管 nft + SQLite，无 TUI）—— systemd 管理的角色：
sudo ./nfuse server --iface ens5 --db /var/lib/nfuse/nfuse.db --socket /run/nfuse.sock

# 从 shell 管理账户和端口（每条都是发给守护进程的一个 RPC）：
sudo ./nfuse add web --tier a --limit 100 --anchor 1   # 100 GiB/月，每月 1 号重置
sudo ./nfuse port add web 60000-60099                  # 计量一个端口范围
sudo ./nfuse list                                      # 人类可读列表
sudo ./nfuse list --json | jq '.[] | {name, used_bytes}'

# 交互式 TUI（连接守护进程的 socket；连不上就报错退出）：
sudo ./nfuse tui --socket /run/nfuse.sock

sudo ./nfuse teardown                                  # 移除规则集
```

当守护进程在 socket 上活跃时，`teardown` 会拒绝运行（它会与运行中的引擎冲突，引擎会在
下一次变更时重建表）。请先停止服务（`systemctl stop nfuse`，或杀掉 `nfuse server`
进程）。

守护进程需要 root（nftables）和 Linux ≥ 5.16。`--iface` 对 **`nfuse server` 是必需的**
（没有默认值）：如果它为空或指向一个主机上不存在的接口，守护进程会拒绝启动——请从
`ip -br link` 中挑一个。`teardown` 和客户端命令不需要它。服务端标志：`--socket`、
`--iface`、`--table`、`--db`、`--sample-interval`、`--persist-interval`、
`--skip-kernel-check`、`--http-addr` / `--http-port`（HTTP 查询端点；**默认关闭**——设置
`--http-port` 来启用它，绑定到 `--http-addr`，默认 `127.0.0.1`）。`nfuse tui` 接受
`--socket` 和 `--ui-refresh`。每个操作类命令都共享 `--socket`。

### 用 `--json` 编写脚本

`nfuse list --json` 向 **stdout** 写出一个稳定的 JSON 数组（日志和错误走 **stderr**，
从不混入其中），因此它可以直接接入 `jq`、计费任务或其它自动化。用量以 **原始字节** 报告
（`used_bytes`），每个端口都带有其数字 **id**——即 `port edit`、`port rm` 和 `port move`
接受的句柄。空账户列表是 `[]`，不是 `null`。成功时退出码为 `0`，失败时为非零，因此脚本
可以据此分支。

```json
[
  {
    "id": 1, "name": "web", "tier": "a",
    "limit_gib": 100, "limit_bytes": 107374182400, "used_bytes": 5368709120,
    "ports": [ { "id": 7, "start": 60000, "end": 60099 } ]
  }
]
```

引用账户的命令接受 **名称或数字 id**：名称先针对实时账户列表解析，含糊的名称（或缺失的
账户）是错误而非猜测。`port` 命令接受 `nfuse list` 显示的数字端口 id。

### HTTP 查询（`curl <host:port>/<token>`）

**服务守护进程** 还可以暴露一个小型 **只读 HTTP 查询端点**，让运维人员或计费脚本用
普通的 `curl` 读取用量——无需 socket，无需客户端二进制。它 **只由 `nfuse server` 启动**
（从不由 `nfuse tui` 或操作类命令），并且 **默认关闭**：只有当你设置 `--http-port` 时它
才启动。绑定地址是 `--http-addr`（默认 `127.0.0.1`），因此即使启用后它也保持仅本地，
直到你有意打开它：

```sh
sudo ./nfuse server --iface ens5 --http-port 8787                     # 启用，仅环回
sudo ./nfuse server --iface ens5 --http-port 8787 --http-addr 0.0.0.0 # 在所有网卡上启用
sudo ./nfuse server --iface ens5                                      # 端点关闭（默认）
```

访问通过 **URL 路径中的令牌**。每个账户都有自己的令牌，还有一个 **主令牌** 一次性返回
所有账户：

```sh
TOK=$(sudo ./nfuse token show web)         # 这个账户的令牌
curl http://127.0.0.1:8787/$TOK            # → 仅这个账户的用量
curl "http://127.0.0.1:8787/$TOK?format=json"   # 机器可读的 JSON（或 ?json）

MTOK=$(sudo ./nfuse token master)          # 主令牌
curl http://127.0.0.1:8787/$MTOK           # → 每个账户的用量
```

- **令牌是 16 个随机的大小写混合字母**，由守护进程生成且 **不可自定义**。
  `nfuse token new <account>` 轮换一个账户的令牌（旧的立即返回 404）；
  `nfuse token master --new` 轮换主令牌。
- **输出格式**：纯文本（默认）或通过 `?format=json` 的 JSON（裸 `?json` 也行）。JSON 始终
  是一个数组——账户令牌一个元素，主令牌每个账户一个元素——并将 `used_bytes`/`limit_bytes`
  报告为原始字节。令牌 **从不** 在响应中回显。
- **未知或空的令牌** 是 `404`；只接受 `GET`/`HEAD`。

**旧数据库透明迁移。** 在令牌存在之前创建的数据库没有 `token` 列也没有主令牌；守护进程
第一次打开它时，会给每个账户一个全新的唯一令牌并生成一个主令牌，**不触碰任何现有的
账户/端口/用量数据**。该步骤是幂等的，因此重启永远不会重新生成令牌。

### 在 systemd 下运行

```ini
[Unit]
Description=Nfuse traffic-metering daemon
After=network-online.target
Wants=network-online.target

[Service]
# --iface 是必需的；替换成你的网卡（见 `ip -br link`）。
ExecStart=/usr/local/bin/nfuse server --iface ens5 --db /var/lib/nfuse/nfuse.db --socket /run/nfuse.sock
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

如果网卡还没起来，守护进程会以非零退出，`Restart=on-failure` 会重试直到它出现——这是
预期行为，而不是去计量错误的（或不存在的）接口。

### TUI 按键

`a` 添加账户 · `d` 删除账户 · `p` 添加端口 · `e` 编辑端口 ·
`x` 删除端口 · `m` 移动端口 · `t` 修改档位 · `r` 重置配额 ·
`u` 设置用量 · `q` 退出

**添加 / 编辑端口** 接受任一形式——单个端口（`60006`）或范围（`60000-60099`）。`e` 编辑
选中的端口行：重新编号一个单端口、平移一个范围的边界、或在两者之间转换，并以当前值预填。
因为内核计数器是以端口的内部 id（而非其号码）为键的，编辑会保留 id，因此 **在改动中保留
该端口累积的 in/out 计数器**。将一个范围滑动到与它 *自身* 的旧区间重叠（例如
`60000-60099` → `60001-60100`）是合法的移动；与 *另一个* 端口重叠会被拒绝。

删除一个仍拥有端口的账户会在一个原子步骤中移除该账户 **及其所有端口**；确认提示会先报出
端口数量。无端口的账户会被直接删除。

**可点击的帮助栏。** 底部的快捷键条是一个 nano 风格的栏：每一项都 **可鼠标点击** 并触发
与其按键完全相同的动作（相同的选中行守卫、相同的错误提示）。当终端太窄放不下一行时，它会
**折行** 到额外的行，栏会增高以适应；当有模态框或表单打开时，对该栏的点击会像任何其它
框外点击一样被覆盖层吞掉。

状态栏显示采样时钟（或以红色显示最近一次错误），外加一行来自 `GetHealth` 的守护进程
信息：受管的 **iface**、**uptime** 和 **last persist** 时间（在第一次快照之前为 `never`）。

## 测试

```sh
go test ./...        # 单元测试始终运行；nft 集成测试需要 root
```

集成测试（`internal/engine`）在 `lo` 上驱动真实的 nftables，当 `nft` 不可用时会自动跳过。

---

<a id="english"></a>

# Nfuse（English）

Nfuse meters **per-port, bidirectional** traffic on a NIC using nftables
**netdev** hooks, persists usage to **SQLite**, and enforces per-account quotas
with an **in-kernel circuit breaker** (nftables named `quota` → per-packet
`drop`). A **subcommand CLI** manages accounts, ports, tiers and resets — with
an interactive **TUI** available as `nfuse tui`.

The metering fast path and the breaker are entirely in the kernel; user space
only samples, persists, resets and reconciles — so if the Go process dies, the
breaker still holds.

## Installation
[For International Users] Use the command below to install Nfuse to your system:
```
sudo bash <(curl -fsSL https://github.com/sketchain/Nfuse/raw/refs/heads/main/nfuse.sh)
```
[For Users in Mainland China] Use the command below to install Nfuse to your system:
```
sudo bash <(curl -fsSL https://gitd.uh.ink/https://github.com/sketchain/Nfuse/raw/refs/heads/main/nfuse_cn.sh)
```


## Command structure

`nfuse` is a subcommand-driven CLI. The **operational** commands are the
first-class citizens: each is a thin RPC client that drives one daemon method
over the socket. The **lifecycle** commands own the process role.

```
nfuse server   --iface X   run the engine daemon (owns nft + SQLite)
nfuse tui                  interactive terminal UI against a running daemon
nfuse teardown             remove the kernel ruleset and exit
nfuse version              print version information

nfuse list [--json]                account/port/usage listing
nfuse add <name> --tier a|b|c …     add an account
nfuse rm <account> [--cascade]      delete an account
nfuse set-tier <account> --tier …   change tier / limit
nfuse reset <account>               zero an account's usage
nfuse set-usage <account> <bytes>   set an account's usage
nfuse persist                       force a SQLite snapshot now
nfuse port add  <account> <start[-end]>
nfuse port edit <port-id> <start[-end]>
nfuse port rm   <port-id>
nfuse port move <port-id> <account>

nfuse token show   <account>        print an account's HTTP query token
nfuse token new    <account>        regenerate an account's query token
nfuse token master [--new]          print (or regenerate) the master token
```

A bare `nfuse` (no subcommand) prints usage and exits non-zero; it no longer
launches the TUI — run `nfuse tui` for that.

### Server / client roles

The same binary runs in one of two roles:

- **`nfuse server`** — the **server daemon**. It runs the engine and listens on
  a Unix domain socket. It is the **only** process that touches nftables and
  SQLite (sampling, persistence, monthly resets, and every mutation go through
  its single reconcile path), so there is no write contention. This is the role
  systemd keeps running.
- **the client commands** (`nfuse tui`, `nfuse list`, `nfuse add`, …) — they
  only connect to the socket and send each action as an RPC. They touch neither
  the kernel nor the DB. If the daemon is not reachable they exit non-zero with
  an error telling you to start the service — there is **no embedded-engine
  fallback**.

The transport is newline-delimited JSON over the socket. Mutating RPCs return
only `ok`/`err`; after a success the client re-reads full state via `GetState`.
All mutations are serialized behind the engine's reconcile lock, shared with the
sampling and reset loops, so the ruleset is never half-updated.

**Single instance.** On startup the daemon probes the socket: if a live daemon
already answers, it **refuses to start** rather than deleting the socket and
stealing it (two engines rebuilding the same nft table would corrupt each
other). Only a stale socket left by an unclean shutdown is reclaimed.

**Client reconnect.** If the daemon is restarted under an open TUI (e.g. systemd
`Restart=on-failure`), a call that hits a connection-level error redials the
socket once and replays the request, so the client recovers instead of dying;
if the daemon is still down the status bar shows the error and the next refresh
retries.

### Cold start vs hot restart

On startup the daemon probes whether its nftables table already exists to decide
the authoritative source of usage:

- **table absent = cold start** (the machine rebooted, kernel state is empty) →
  **SQLite is authoritative**: build the table seeding counters/quotas from the DB.
- **table present = hot restart** (the process was relaunched, e.g. by
  `Restart=on-failure`, but the box stayed up) → **the kernel is authoritative**:
  sample the live values, fold them into SQLite, and rebuild seeding from those,
  so a stale DB never overwrites usage the kernel is still tracking.

## How it works

### Metering (kernel, netdev @ NIC)

Two device-bound chains are attached to the managed interface (`--iface`, e.g.
`ens5`):

- **ingress** hook — matches **destination port** `P` (external → local port `P`)
- **egress** hook — matches **source port** `P` (local port `P` → external)

Each managed port gets **one counter per direction** (`in` = ingress/dport,
`out` = egress/sport), so the TUI can show a per-port, per-direction breakdown.
The egress hook requires **Linux ≥ 5.16**; Nfuse checks this at startup.

#### Port ranges

A managed entry may be a single port (`60006`) or a **contiguous range**
(`60000-60099`), entered in that `start-end` form (surrounding blanks such as
`60000 - 60099` are tolerated). A range is metered and circuit-broken **as one
whole**: it renders as nftables' native range match (`th dport 60000-60099`) and
gets **exactly one counter pair** (in/out) for the entire span — there is no
per-port breakdown within a range. Its bytes fold into the owning account's
shared quota just like any other port.

Ranges (and single ports) must **not overlap** — anywhere, across all accounts.
Adjacent ranges are fine (`60000-60099` and `60100-60199` coexist); any shared
port number is rejected. The check is enforced atomically inside the engine's
reconcile path (so two concurrent adds can't both slip an overlapping range in)
and again in `Snapshot.Validate` as the last line before every kernel apply. A
range is well-formed when `1 ≤ start ≤ end ≤ 65535`.

### Circuit breaking (kernel, named quota)

All ports of one account reference the **same named `quota` object**, so the
kernel sums that account's in + out bytes with **equal weight** into a single
budget. Each rule is laid out as:

```
<match> quota name "acct<id>" drop      # breaker: evaluated first
<match> counter name "p<portID>_<dir>"  # detail: only reached if not dropped
```

While under budget the quota statement evaluates false, the `drop` is skipped,
and the counter rule runs. Once the budget is exceeded the quota yields `drop`,
which terminates rule evaluation — so **the counter sits after the drop and
stops advancing the instant the breaker trips**. Drops are per-packet with no
graceful handling: new and established connections are treated identically.
Unlimited accounts install **no quota rule** and never break.

This is verified empirically: with a 50 KB quota, a client attempting to send
2.6 MB over loopback times out once tripped, the quota caps at `used 50000`, and
the post-drop counter freezes below the limit.

### Account tiers

| Tier | Meaning              | Kernel quota | User-space reset      |
|------|----------------------|--------------|-----------------------|
| `a`  | X GiB per month      | `over X`     | monthly (billing day) |
| `b`  | X GiB one-shot       | `over X`     | never (permanent)     |
| `c`  | unlimited            | none         | n/a                   |

Tiers **a** and **b** are byte-for-byte identical in the kernel ("drop once over
X"). The *only* difference is whether user space performs a periodic reset. The
billing anchor day must be **1–28** (values outside that range are rejected, not
clamped, so a mistyped day never silently changes the cycle).

**Switching tiers does not clear usage — by design.** Moving an account from a/b
to **c** only *pauses* metering (the quota object is removed); moving it back
revives the historical `used_bytes` and re-seeds it into the kernel quota. This
is intentional, not a bug: tier c is "billing paused", not "start over". To
actually zero usage, reset the account (`r`).

### Persistence & reboot backfill (SQLite, WAL)

Kernel quota/counter values are **lost on reboot**. Nfuse periodically snapshots
each account's quota usage and every counter to SQLite (WAL mode). On startup it
reloads those values and **seeds** the freshly created nft objects — the quota
via `used <bytes>` and counters via `packets/bytes` — so tier a/b enforcement
stays accurate across restarts. This is validated by a test that persists 500
MiB, "restarts", and confirms the rebuilt quota reports the same `used`.

Writes run on their own goroutine/ticker reading the latest in-memory sample, so
**SQLite writes never block sampling or the breaker**.

### Object lifecycle

Every mutation (add/delete account or port, tier change) is applied by **full
atomic reconciliation**: take a **fresh** live sample → fold it into SQLite →
rebuild the whole table from a single `nft -f` script (`add table; delete table;
add table`). This creates/reclaims counters, quotas and rules correctly without
hand-tracking rule handles, and can't leave the ruleset half-updated. The sample
is taken *at reconcile time* (not reused from the periodic sampler), so the
traffic metered between the last periodic sample and the rebuild is not lost; if
the live table can't be sampled the mutation is refused rather than rebuilt from
stale accounting.

A monotonic generation counter guards against a slow periodic sample racing a
usage-lowering mutation (reset, monthly reset, set-usage): a sample taken before
such a mutation completes is discarded instead of writing the pre-change value
back over it.

## Why `nft` instead of a netlink library

Nfuse shells out to `nft(8)` rather than using `google/nftables`:

- The whole ruleset is applied from one `nft -f` script, which nft runs as a
  single **atomic transaction** — race-free full reconciliation for free.
- `nft -j list` returns **structured JSON**, making counter/quota sampling a
  stable parse instead of manual netlink attribute decoding.
- Tiny dependency surface, and it matches how an operator inspects the ruleset.

The cost (a process per apply/sample) is irrelevant: applies happen only on
mutation and sampling runs at human timescales.

## Layout

```
main.go                 thin shim: inject build metadata, hand off to internal/cli
internal/cli            subcommand dispatch, operational commands, --json output
internal/model          domain types + invariants (account→port→counter tree)
internal/store          SQLite (WAL) load/save + reboot backfill
internal/nft            nftables manager: script generation + JSON sampling
internal/engine         sampling loop, non-blocking persistence, resets, reconcile
internal/rpc            Unix-socket JSON RPC: server (engine) + client (consumers)
internal/httpapi        read-only HTTP query endpoint (curl <host:port>/<token>)
internal/system         kernel-version preflight (netdev egress ≥ 5.16)
internal/tui            tview UI (drives a local engine or the RPC client)
```

## Usage

```sh
go build -o nfuse .

# Server daemon (owns nft + SQLite, no TUI) — the systemd-managed role:
sudo ./nfuse server --iface ens5 --db /var/lib/nfuse/nfuse.db --socket /run/nfuse.sock

# Manage accounts and ports from the shell (each is one RPC to the daemon):
sudo ./nfuse add web --tier a --limit 100 --anchor 1   # 100 GiB/month, resets on the 1st
sudo ./nfuse port add web 60000-60099                  # meter a port range
sudo ./nfuse list                                      # human-readable listing
sudo ./nfuse list --json | jq '.[] | {name, used_bytes}'

# Interactive TUI (connects to the daemon's socket; errors out if it can't):
sudo ./nfuse tui --socket /run/nfuse.sock

sudo ./nfuse teardown                                  # remove the ruleset
```

`teardown` refuses to run while a daemon is live on the socket (it would fight
the running engine, which rebuilds the table on its next mutation). Stop the
service first (`systemctl stop nfuse`, or kill the `nfuse server` process).

The daemon requires root (nftables) and Linux ≥ 5.16. `--iface` is **required
for `nfuse server`** (there is no default): the daemon refuses to start if it is
empty or names an interface that doesn't exist on the host — pick one from
`ip -br link`. `teardown` and the client commands don't need it. Server flags:
`--socket`, `--iface`, `--table`, `--db`, `--sample-interval`,
`--persist-interval`, `--skip-kernel-check`, `--http-addr` / `--http-port`
(HTTP query endpoint; **off by default** — set `--http-port` to enable it, bound
to `--http-addr`, default `127.0.0.1`). `nfuse tui` takes `--socket` and
`--ui-refresh`. Every operational command shares `--socket`.

### Scripting with `--json`

`nfuse list --json` writes a stable JSON array to **stdout** (logs and errors go
to **stderr**, never mixed in), so it drops straight into `jq`, billing jobs or
other automation. Usage is reported as **raw bytes** (`used_bytes`), and every
port carries its numeric **id** — the handle `port edit`, `port rm` and
`port move` take. An empty account list is `[]`, not `null`. Exit code is `0` on
success and non-zero on failure so scripts can branch on it.

```json
[
  {
    "id": 1, "name": "web", "tier": "a",
    "limit_gib": 100, "limit_bytes": 107374182400, "used_bytes": 5368709120,
    "ports": [ { "id": 7, "start": 60000, "end": 60099 } ]
  }
]
```

Account-referencing commands accept a **name or a numeric id**: the name is
resolved first against the live account list, and an ambiguous name (or a
missing account) is an error rather than a guess. `port` commands take the
numeric port id shown by `nfuse list`.

### HTTP query (`curl <host:port>/<token>`)

The **server daemon** can also expose a small **read-only HTTP query endpoint** so
an operator or billing script can read usage with plain `curl` — no socket, no
client binary. It is started **only by `nfuse server`** (never by `nfuse tui` or
the operational commands) and is **off by default**: it starts only when you set
`--http-port`. The bind address is `--http-addr` (default `127.0.0.1`), so even
once enabled it stays local-only until you deliberately open it:

```sh
sudo ./nfuse server --iface ens5 --http-port 8787                     # enable, loopback only
sudo ./nfuse server --iface ens5 --http-port 8787 --http-addr 0.0.0.0 # enable on all NICs
sudo ./nfuse server --iface ens5                                      # endpoint off (default)
```

Access is by **token in the URL path**. Every account has its own token, and there
is one **master token** that returns every account at once:

```sh
TOK=$(sudo ./nfuse token show web)         # this account's token
curl http://127.0.0.1:8787/$TOK            # → just this account's usage
curl "http://127.0.0.1:8787/$TOK?format=json"   # machine-readable JSON (or ?json)

MTOK=$(sudo ./nfuse token master)          # the master token
curl http://127.0.0.1:8787/$MTOK           # → every account's usage
```

- **Tokens are 16 random mixed-case letters**, generated by the daemon and **not
  customizable**. `nfuse token new <account>` rotates an account's token (the old
  one 404s immediately); `nfuse token master --new` rotates the master token.
- **Output formats**: plain text (default) or JSON via `?format=json` (bare `?json`
  works too). The JSON is always an array — one element for an account token, one
  per account for the master token — and reports `used_bytes`/`limit_bytes` as raw
  bytes. A token is **never** echoed back in a response.
- An **unknown or empty token** is `404`; only `GET`/`HEAD` are accepted.

**Legacy databases migrate transparently.** A database created before tokens
existed has no `token` column and no master token; the first time the daemon opens
it, every account is given a fresh unique token and a master token is generated,
**without touching any existing account/port/usage data**. The step is idempotent,
so restarts never re-roll the tokens.

### Running under systemd

```ini
[Unit]
Description=Nfuse traffic-metering daemon
After=network-online.target
Wants=network-online.target

[Service]
# --iface is required; substitute your NIC (see `ip -br link`).
ExecStart=/usr/local/bin/nfuse server --iface ens5 --db /var/lib/nfuse/nfuse.db --socket /run/nfuse.sock
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

If the NIC isn't up yet the daemon exits non-zero and `Restart=on-failure`
retries until it appears — the intended behavior rather than metering the wrong
(or no) interface.

### TUI keys

`a` add account · `d` delete account · `p` add port · `e` edit port ·
`x` delete port · `m` move port · `t` change tier · `r` reset quota ·
`u` set usage · `q` quit

**Adding / editing a port** accepts either form — a single port (`60006`) or a
range (`60000-60099`). `e` edits the selected port row: renumber a single port,
shift a range's bounds, or convert between the two, pre-filled with the current
value. Because kernel counters are keyed by the port's internal id (not its
number), an edit keeps the id and so **preserves the port's accumulated
in/out counters** across the change. Sliding a range so it overlaps *its own*
old extent (e.g. `60000-60099` → `60001-60100`) is a legal move; overlapping a
*different* port is rejected.

Deleting an account that still owns ports removes the account **and all of its
ports** in one atomic step; the confirmation prompt names the port count first.
A portless account is deleted directly.

**Clickable help bar.** The shortcut strip at the bottom is a nano-style bar:
every item is **mouse-clickable** and triggers exactly the same action as its
key (same selected-row guard, same error prompts). When the terminal is too
narrow to fit the strip on one line it **wraps** onto extra rows and the bar
grows to fit; while a modal or form is open, clicks on the bar are swallowed by
the overlay just like any other click outside it.

The status bar shows the sampling clock (or the last error in red) plus a daemon
line from `GetHealth`: managed **iface**, **uptime**, and **last persist** time
(`never` until the first snapshot).

## Testing

```sh
go test ./...        # unit tests always run; nft integration tests need root
```

Integration tests (`internal/engine`) drive real nftables on `lo` and are
skipped automatically when `nft` is unavailable.
</content>
</invoke>
