package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Peer 描述一个通信对端。
// Session 为 opencode 会话 ID（可为空，表示仅使用文件邮箱）。
type Peer struct {
	Session string `json:"session,omitempty"` // ses_xxx
	Dir     string `json:"dir,omitempty"`     // 对端工作目录（用于展示与创建会话）
	Desc    string `json:"desc,omitempty"`    // 人类可读描述
	Agent   string `json:"agent,omitempty"`   // 注入时使用的 agent，可空
	Model   string `json:"model,omitempty"`   // 注入时使用的模型，可空
}

// OpencodeConf 保存与 opencode 服务通信的配置。
type OpencodeConf struct {
	// StateFile 指向 opencode 的状态文件（含 url 与 password）。
	// 留空则自动探测 %USERPROFILE%\.local\state\opencode\service.json
	StateFile string `json:"stateFile,omitempty"`
	// CLIPath 为回退使用的 opencode-cli.exe 路径，留空则自动探测。
	CLIPath string `json:"cliPath,omitempty"`
}

// Config 是 relay 的持久化配置。
type Config struct {
	Self     string          `json:"self"`
	Mailbox  string          `json:"mailbox"`
	Peers    map[string]Peer `json:"peers"`
	Bridge   BridgeConfig    `json:"bridge"`
	Opencode OpencodeConf    `json:"opencode"`

	path string // 配置来源文件，不参与序列化
}

// exeDir 返回当前可执行文件所在目录。
func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(p)
}

// defaultMailbox 返回默认邮箱目录（工程内 _relay\mailbox）。
func defaultMailbox() string {
	// exe 位于 <root>\toolchain\relay\relay.exe，邮箱位于 <root>\toolchain\_relay\mailbox
	base := exeDir()
	parent := filepath.Dir(base)
	return filepath.Join(parent, "_relay", "mailbox")
}

// findConfigPath 按优先级定位配置文件。
func findConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("RELAY_CONFIG"); v != "" {
		return v
	}
	cands := []string{
		filepath.Join(exeDir(), "relay.json"),
		filepath.Join(filepath.Dir(exeDir()), "relay.json"),
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "relay.json"))
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return filepath.Join(exeDir(), "relay.json")
}

// loadConfig 读取配置；文件不存在时返回带默认值的空配置。
func loadConfig(explicit string) (*Config, error) {
	p := findConfigPath(explicit)
	c := &Config{
		Self:    "workbuddy",
		Mailbox: defaultMailbox(),
		Peers:   map[string]Peer{},
		path:    p,
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("解析配置失败 %s: %w", p, err)
	}
	c.path = p
	if c.Self == "" {
		c.Self = "workbuddy"
	}
	if c.Mailbox == "" {
		c.Mailbox = defaultMailbox()
	}
	if c.Peers == nil {
		c.Peers = map[string]Peer{}
	}
	return c, nil
}

// Save 将配置原子写回磁盘。
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// Path 返回配置文件的落盘位置。
func (c *Config) Path() string { return c.path }

// LookupPeer 查找对端，名字不区分大小写。
func (c *Config) LookupPeer(name string) (string, Peer, bool) {
	for k, v := range c.Peers {
		if strings.EqualFold(k, name) {
			return k, v, true
		}
	}
	return "", Peer{}, false
}

// serviceState 是 opencode 本地服务的状态文件结构。
type serviceState struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	URL      string `json:"url"`
	PID      int    `json:"pid"`
	Password string `json:"password"`
}

// stateFileCandidates 列出状态文件的候选路径。
func stateFileCandidates(conf OpencodeConf) []string {
	var out []string
	if conf.StateFile != "" {
		out = append(out, conf.StateFile)
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, ".local", "state", "opencode", "service.json"),
			filepath.Join(home, ".config", "opencode", "service.json"),
		)
	}
	if v := os.Getenv("RELAY_STATE_FILE"); v != "" {
		out = append(out, v)
	}
	return out
}

// resolveService 定位 opencode 服务地址与凭据。
// 优先读状态文件；状态缺失时回退到 opencode-cli.exe service status 获取地址。
func (c *Config) resolveService() (string, string, error) {
	var lastErr error
	for _, p := range stateFileCandidates(c.Opencode) {
		data, err := os.ReadFile(p)
		if err != nil {
			lastErr = err
			continue
		}
		var st serviceState
		if err := json.Unmarshal(data, &st); err != nil {
			lastErr = fmt.Errorf("解析 %s 失败: %w", p, err)
			continue
		}
		if st.URL != "" && st.Password != "" {
			return st.URL, st.Password, nil
		}
		// 配置文件里可能只有 password（无 url），此时需向 CLI 询问地址
		if st.Password != "" {
			url, err := cliServiceStatus(c)
			if err == nil && url != "" {
				return url, st.Password, nil
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("未找到 opencode 服务状态文件")
	}
	return "", "", lastErr
}

// ocCliPath 定位 opencode-cli.exe。
func ocCliPath(c *Config) string {
	if c.Opencode.CLIPath != "" {
		return c.Opencode.CLIPath
	}
	if v := os.Getenv("RELAY_OPENCODE_CLI"); v != "" {
		return v
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		p := filepath.Join(la, "Programs", "@opencodedesktop", "resources", "opencode-cli.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
