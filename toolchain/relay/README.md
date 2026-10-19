# relay —— opencode 会话间接力桥

让两个不同项目目录下的 AI 会话（例如 WorkBuddy 侧与 GLM 侧）**互相收发指令与结果**，
不需要人来回复制粘贴。

- 单个 `relay.exe`，**6.8 MB，零运行时依赖**（不需要 Python / Node / .NET）
- 直连本机 opencode 服务，单次调用 **4–8 ms**
- 消息**落邮箱 + 注入对端会话**双通道：对端不在线也不丢消息

---

## 一、它解决什么问题

原先的交互模式：

```
人：把任务书复制给 GLM
GLM：干完活，把结果复制回 WorkBuddy
```

现在：

```
WorkBuddy：relay send glm --file 任务书.md
GLM：      干完活 → relay reply deepseek "已完成，产物在 output\xxx"
WorkBuddy：relay inbox      ← 直接读到 GLM 的回传
```

两边都是**工具主动投递**，不再需要人工中转。

---

## 二、快速开始

```bash
cd D:\待办事项优先级提示窗\toolchain\relay

# 1) 看看有哪些会话
relay.exe ls

# 2) 把 GLM 的会话登记为对端（把 ses_xxx 换成实际 ID）
relay.exe bind glm ses_xxxxxxxx --dir "D:\待办事项优先级提示窗" --desc "GLM5.3flash"

# 3) 发一条任务
relay.exe send glm "请按 output\开发任务书.md 执行，完成后回传" --subject "Go 重写任务"

# 4) 查看收件
relay.exe inbox
```

---

## 三、双向协作的完整用法

### WorkBuddy 侧（本端，身份 = deepseek）

```bash
# 发任务（长文档用 --file，避免命令行长度限制）
relay.cmd send glm --file "D:\...\开发任务书-致GLM5.3flash-Go重写.md" --subject "Go 重写任务"

# 发完就地等它干完，直接把 GLM 的回复打印出来（一条命令闭环）
relay.cmd ask glm --file "开发任务书.md" --timeout 1800

# 或者稍后回来查收
relay.cmd inbox          # 只看未读
relay.cmd inbox --all    # 看全部
relay.cmd read <消息ID>  # 看全文
relay.cmd ack --all      # 全部标记已读
```

### GLM 侧（身份 = glm）

GLM 在自己的会话里干完活后，执行：

```bash
cd D:\待办事项优先级提示窗\toolchain\relay
relay-glm.cmd reply deepseek "已完成 Go 重写。产物：output\relay\...。测试 74/74 通过。" --ref m1791799380091251d
```

`relay-glm.cmd` 是包装脚本，已把身份固定成 `glm`。
**请务必用 `relay-glm.cmd`**，不要直接用 `relay.exe`——否则回传消息会被署名成 `deepseek`。

> 回传时带上 `--ref <收到的任务消息ID>`，这样两边能对上号，形成可追溯的任务链。

---

## 四、命令参考

| 命令 | 作用 |
|---|---|
| `ls` | 列出全部 opencode 会话（ID / 状态 / 标题 / 目录 / 模型 / 最后活动） |
| `bind <名字> <会话ID>` | 登记对端，可选 `--dir` `--desc` |
| `unbind <名字>` | 解除绑定 |
| `peers` | 查看已登记对端及其实时忙闲状态 |
| `self` | 显示本端身份、邮箱位置、服务连接情况 |
| `init` | 生成/更新配置文件 |
| `send <对端> <内容>` | 发消息：落邮箱 + 注入对端会话 |
| `reply <对端> <内容>` | 回传结果（等价 `send --kind reply`） |
| `push <对端> [--all]` | 把邮箱里尚未注入的消息补投到对端会话 |
| `inbox [--all]` | 查看收件箱 |
| `read <消息ID>` | 查看消息全文 |
| `ack <消息ID...>` / `--all` | 标记已读 |
| `wait <对端>` | 等待对端处理完毕并打印其回复 |
| `ask <对端> <内容>` | 发送 + 等待 + 打印回复 + 归档（推荐） |
| `status` | 总览：身份、未读数、对端状态 |

### 常用参数

| 参数 | 说明 |
|---|---|
| `--self <名字>` | 声明本次调用代表谁（对端回传时用） |
| `--file <路径>` | 从文件读取正文（长文档推荐） |
| `--subject <文本>` | 消息摘要 |
| `--kind task\|reply\|note` | 消息类型，默认 `task` |
| `--ref <消息ID>` | 关联上一条消息 |
| `--timeout <秒>` | 等待超时，`wait` 默认 600 / `ask` 默认 900 |
| `--since <消息ID>` | `wait` 的起点，默认取当前时刻 |
| `--no-push` | 只落邮箱，不注入对端会话 |
| `--json` | 机器可读输出 |
| `--config <路径>` | 指定配置文件 |

---

## 五、工作原理

### 双通道投递

| 通道 | 落点 | 特点 |
|---|---|---|
| **邮箱** | `toolchain\_relay\mailbox\*.json` | 一定成功；对端离线也不丢；可追溯、可重放 |
| **会话注入** | 对端 opencode 会话 | 对端立刻看到并开始干活 |

`send` 会同时走两条路。注入失败（对端未绑定 / 服务未启动）不影响邮箱落盘，
之后可用 `push` 补投。

### 如何判定"对端干完了"

注入消息后，工具轮询对端会话消息流。opencode 在每轮处理结束时会写一条
`idle` 类型消息，且 assistant 回复会带 `completed` 时间戳——`relay wait/ask`
就以"出现本次任务之后的新 `idle`"作为完成信号，而不是盲目等待固定时长。

### 与 opencode 的通信方式

直连本机 opencode 后台服务（HTTP）：

```
GET  /api/session                      列会话
GET  /api/session/{id}/message         读消息
POST /api/session/{id}/prompt          投递消息
POST /api/session/{id}/interrupt       打断
```

服务地址与密码自动从本机状态文件读取：

```
%USERPROFILE%\.local\state\opencode\service.json
```

认证方式为 HTTP Basic（用户名任意，密码即服务密码）。
若状态文件不可用，会回退调用 `opencode-cli.exe service status` 取地址。

### 与 ai-bridge 互操作（三向互通）

除自家邮箱外，relay 还能读写 MiMo 建立的 **ai-bridge 文件桥**
（`C:\Users\67462\Documents\画师串识别软件开发\ai-bridge\`），实现三方互通。

配置在 `relay.json` 的 `bridge` 段：

```json
"bridge": {
  "enabled": true,
  "root": "C:\\Users\\67462\\Documents\\画师串识别软件开发\\ai-bridge",
  "links": {
    "mimo": { "writeDir": "to-mimo", "readDir": "to-deepseek", "theirPrefix": "M", "myPrefix": "D" }
  }
}
```

- **导出**：`relay send mimo ...` 除写邮箱、注入会话外，还会按 ai-bridge 的
  YAML 头 Markdown 格式**镜像一份到 `to-mimo/`**，编号自动递增（`D-0001`、`D-0002`…），
  并以 **UTF-8 BOM** 写入——因为对方的 `bridge.ps1` 用 `Set-Content -Encoding UTF8`，
  PowerShell 5.1 会带 BOM，且其读取端按 ANSI 解析无 BOM 文件会乱码。
- **导入**：`relay inbox` 会先扫描 `to-deepseek/`，把新消息并入 relay 邮箱，
  **并自动写 `ack/<ID>.ok`**（ai-bridge 协议规定读方负责回执）。导入按桥 ID 去重，幂等。
- **引用互认**：`resolveRef` 会把 relay 内部 ID 翻译成对端能识别的桥 ID 填进 `reply_to`。
- **可选扩展字段**：写出的文件带 `relay_id` 与 `kind`，对方不认识会自然忽略。

**防冲突的两条硬约定**：

1. **一个目录只有一方写** —— `to-mimo/` 只有 relay 写，`to-deepseek/` 只有 MiMo 写
2. **编号前缀互斥** —— `D-` 归 relay，`M-` 归 MiMo，两边各自扫目录取 max+1，永不撞号

> 事实源约定：**ai-bridge 目录是三方的共享事实源**，relay 邮箱只是本端内部记录。

---

## 六、文件位置

```
toolchain\relay\
├── relay.exe        工具本体（唯一需要的文件）
├── relay.cmd        WorkBuddy 侧入口（身份=deepseek）
├── relay-glm.cmd    GLM 侧入口（身份=glm）
├── relay.json       配置：身份、邮箱路径、对端登记
└── src\             Go 源码（纯标准库，可自行改）

toolchain\_relay\
└── mailbox\         消息邮箱（每条约 300 字节的 JSON）
```

---

## 七、安全边界

- 工具**只与本机 `127.0.0.1` 上的 opencode 服务通信**，不对外发起任何网络请求。
- 服务密码从本机状态文件读取后**仅用于当次请求**，不写入 `relay.json`、不打印到输出。
- 邮箱文件是明文 JSON，位于项目内 `toolchain\_relay\mailbox`，**不要把密钥类内容写进消息正文**。
- `send` 的正文会成为对端会话里的用户消息，对端 AI 会读到——不要发送你不希望被记录的内容。
- 建议保持邮箱目录在项目内，不要指向个人目录或系统目录。

---

## 八、故障排查

| 现象 | 处理 |
|---|---|
| `连接 opencode 服务失败` | opencode 后台服务没起。执行 `opencode-cli service status` 确认，或先启动 opencode 桌面端 |
| `未找到 opencode 服务状态文件` | 用 `--config` 指定配置，或在 `relay.json` 的 `opencode.stateFile` 写明状态文件路径 |
| 发出去但对方没反应 | `relay ls` 看对端会话状态是否为"运行中"；`relay push <对端>` 补投；确认对端已 `bind` |
| 回传消息署名错了 | 用了 `relay.exe` 而不是 `relay-glm.cmd`，补上 `--self glm` |
| `wait` 一直超时 | 对端任务耗时超过 `--timeout`，加大超时值；或先用 `inbox` 看是否已回传 |
| 中文输出乱码 | 用 Windows Terminal / Git Bash；工具已自动把控制台切到 UTF-8 |

---

## 九、重新编译

```bash
cd D:\待办事项优先级提示窗\toolchain\relay\src
source ../../goenv.sh          # 项目内 Go 1.27.2，隔离 GOPATH/GOCACHE
go build -ldflags "-s -w" -o ../relay.exe .
```

无第三方依赖，仅用 Go 标准库。
