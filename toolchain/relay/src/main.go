package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

const version = "1.0.0"

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procConsoleCP   = kernel32.NewProc("SetConsoleOutputCP")
	procConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// initConsole 将控制台代码页切到 UTF-8，避免中文输出乱码。
func initConsole() {
	if err := procConsoleCP.Find(); err == nil {
		_, _, _ = procConsoleCP.Call(uintptr(65001))
	}
}

func printHelp() {
	fmt.Print(`relay ` + version + ` - opencode 会话间接力桥（跨项目互相访问）

用法:
  relay <命令> [参数] [--flags]

会话与对端
  ls, list              列出全部 opencode 会话（含目录、状态、最后活动）
  bind <名字> <会话ID>   把某个会话登记为对端，例：
                          relay bind glm ses_xxx --dir D:\待办事项优先级提示窗 --desc "GLM5.3flash"
                          relay bind glm --latest --dir "D:\待办事项优先级提示窗"  自动选最新会话
  unbind <名字>          解除对端绑定
  peers                 查看已登记的对端及其实时状态
  self, whoami          显示本端身份、邮箱位置与服务连接情况
  init                  生成/更新配置文件

消息收发
  send <对端> <内容>     发送消息：落邮箱 + 注入对端会话
  reply <对端> <内容>    回传结果（等价于 send --kind reply）
  push <对端> [--all]    把邮箱里尚未注入的消息补投到对端会话
  inbox [--all]          查看收件箱（默认只看未读）
  read <消息ID>          查看消息全文
  ack <消息ID...>|--all  标记已读

等待与闭环
  wait <对端>            等待对端处理完毕并打印其回复
  ask <对端> <内容>      发送 + 等待 + 打印回复 + 归档（一条命令走完闭环）
  status                总览：身份、未读数、对端状态

常用参数
  --self <名字>          声明本次调用代表谁（双方共用配置时对端回传必填）
                         也可设环境变量 RELAY_SELF，或用 relay-glm.cmd 包装脚本
  --file <路径>          从文件读取消息正文（长文档推荐）
  --subject <文本>       消息摘要
  --kind task|reply|note 消息类型（默认 task）
  --ref <消息ID>         关联上一条消息
  --timeout <秒>         等待超时，默认 wait=600 / ask=900
  --since <消息ID>       wait 的起点，默认取当前时刻
  --no-push              只落邮箱，不注入对端会话
  --json                 机器可读输出
  --config <路径>        指定配置文件

典型闭环
  1) 我发给 GLM:
       relay send glm --file 开发任务书.md --subject "Go 重写任务"
  2) GLM 干完活，在它的会话里回传:
       relay reply workbuddy "已完成，产物在 output\xxx" --ref <消息ID>
  3) 我查看结果:
       relay inbox          然后  relay read <消息ID>

说明
  - 邮箱是磁盘上的 JSON 文件，因此“落邮箱”始终成功，不依赖对端在线。
  - 注入依赖 opencode 后台服务；服务地址与凭据自动从本机状态文件发现。
  - 邮箱位置：<工程>\toolchain\_relay\mailbox
`)
}

// 全局参数可出现在命令前后任意位置，先统一抽取，避免 --self glm reply ... 被误判为子命令。
var globalFlags = map[string]bool{"self": true, "config": true, "json": true}
var globalWithValue = map[string]bool{"self": true, "config": true}

func main() {
	initConsole()
	args := os.Args[1:]
	if len(args) == 0 {
		printHelp()
		return
	}

	globals := map[string]string{}
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			rest = append(rest, a)
			continue
		}
		name, val := a[2:], ""
		hasVal := false
		if j := strings.IndexByte(name, '='); j >= 0 {
			name, val, hasVal = name[:j], name[j+1:], true
		}
		if !globalFlags[name] {
			rest = append(rest, a)
			continue
		}
		if !hasVal {
			if globalWithValue[name] && i+1 < len(args) {
				val = args[i+1]
				i++
			} else {
				val = "true"
			}
		}
		globals[name] = val
	}

	if len(rest) == 0 {
		printHelp()
		return
	}
	cmd := rest[0]
	if cmd == "-h" || cmd == "--help" || cmd == "help" || cmd == "-help" {
		printHelp()
		return
	}
	if cmd == "-v" || cmd == "--version" || cmd == "version" {
		fmt.Printf("relay %s\n", version)
		return
	}

	pos, flags := parseArgs(rest[1:])
	for k, v := range globals {
		if _, ok := flags[k]; !ok {
			flags[k] = v
		}
	}

	cfg, err := loadConfig(flags["config"])
	if err != nil {
		fail(err)
	}
	// 身份覆盖：双方共用一份配置时，用 --self 或 RELAY_SELF 声明自己是谁。
	// 否则对端回传的消息会被错误地标记成配置里 self 的身份。
	if v := flags["self"]; v != "" {
		cfg.Self = v
	} else if v := os.Getenv("RELAY_SELF"); v != "" {
		cfg.Self = v
	}
	mb, err := NewMailbox(cfg.Mailbox)
	if err != nil {
		fail(err)
	}
	app := &App{cfg: cfg, mb: mb, jsonOut: flagBool(flags, "json")}

	switch strings.ToLower(cmd) {
	case "init":
		err = cmdInit(app, pos, flags)
	case "self", "whoami":
		err = cmdSelf(app, pos, flags)
	case "ls", "list", "sessions":
		err = cmdLs(app, pos, flags)
	case "bind":
		err = cmdBind(app, pos, flags)
	case "unbind":
		err = cmdUnbind(app, pos, flags)
	case "peers":
		err = cmdPeers(app, pos, flags)
	case "send", "s":
		err = cmdSend(app, pos, flags)
	case "reply", "re":
		err = cmdReply(app, pos, flags)
	case "push":
		err = cmdPush(app, pos, flags)
	case "inbox", "in":
		err = cmdInbox(app, pos, flags)
	case "read", "r":
		err = cmdRead(app, pos, flags)
	case "ack":
		err = cmdAck(app, pos, flags)
	case "wait":
		err = cmdWait(app, pos, flags)
	case "ask":
		err = cmdAsk(app, pos, flags)
	case "status", "st":
		err = cmdStatus(app, pos, flags)
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		printHelp()
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "relay 错误: %v\n", err)
	os.Exit(1)
}
