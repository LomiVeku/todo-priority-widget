//go:build windows

// store.go —— 持久化层：待办数据 + 图片附件（app/src/main/store.js 的 Go 重写）。
//
// 默认数据目录：%APPDATA%\待办事项优先级提示窗-Go
// 若可执行文件同级存在 data 目录，则自动切到"便携模式"，数据随程序走。
//
// 护栏（与 Electron 版一致）：
//   1) 写入 = 临时文件 + rename（同目录原子替换）；
//   2) todos.json 与 window.json 互相独立，互不覆盖；
//   3) 孤儿图片回收双重护栏：只清理 mtime 超 7 天的文件；
//      数据文件读取失败时 lastLoadOk=false，调用方必须跳过回收。
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Store struct {
	baseDir    string
	mediaDir   string
	dataFile   string
	tmpFile    string
	windowFile string
	windowTmp  string
	portable   bool
	lastLoadOk bool
}

func NewStore(defaultDir, portableRoot string) *Store {
	s := &Store{baseDir: defaultDir, lastLoadOk: true}
	if portableRoot != "" {
		pd := filepath.Join(portableRoot, "data")
		if st, err := os.Stat(pd); err == nil && st.IsDir() {
			s.baseDir = pd
			s.portable = true
		}
	}
	s.mediaDir = filepath.Join(s.baseDir, "media")
	s.dataFile = filepath.Join(s.baseDir, "todos.json")
	s.tmpFile = filepath.Join(s.baseDir, "todos.json.tmp")
	s.windowFile = filepath.Join(s.baseDir, "window.json")
	s.windowTmp = filepath.Join(s.baseDir, "window.json.tmp")
	s.ensureDirs()
	return s
}

func (s *Store) ensureDirs() {
	for _, d := range []string{s.baseDir, s.mediaDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			appLogf("store: 创建目录失败 %s: %v", d, err)
		}
	}
}

// Load：读 todos.json。
// 读取失败（非"文件不存在"）/ 结构不合法 / JSON 损坏 → 回退空数据并置 lastLoadOk=false；
// 仅"文件不存在"（真·首次使用）视为正常，置 true。
func (s *Store) Load() (map[string]interface{}, bool) {
	raw, err := os.ReadFile(s.dataFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.lastLoadOk = true // 首次使用不算损坏
			return map[string]interface{}{"todos": []interface{}{}, "settings": map[string]interface{}{}}, true
		}
		// 读取失败（权限拒绝 / 文件被占用 / 磁盘 I/O 错误）必须按"加载失败"处理：
		// 若误判为"首次使用"置 true，渲染层将执行图片回收（app.js:1313），
		// 而此时 todos 已被回退为空 → 引用集为空 → 超龄图片（>7 天）被删光。
		// 2026-10-14 修复：原实现把该分支与"首次使用"合并，构成静默数据丢失路径。
		appLogf("store: 读取失败 %v", err)
		s.lastLoadOk = false
		return map[string]interface{}{
			"todos": []interface{}{}, "settings": map[string]interface{}{},
			"loadError": err.Error(),
		}, false
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		appLogf("store: 数据文件损坏，回退为空（%v）", err)
		s.lastLoadOk = false
		return map[string]interface{}{
			"todos": []interface{}{}, "settings": map[string]interface{}{},
			"loadError": err.Error(),
		}, false
	}
	if _, ok := data["todos"].([]interface{}); !ok {
		appLogf("store: 结构不合法（todos 非数组）")
		s.lastLoadOk = false
		return map[string]interface{}{
			"todos": []interface{}{}, "settings": map[string]interface{}{},
			"loadError": "结构不合法",
		}, false
	}
	if _, ok := data["settings"].(map[string]interface{}); !ok {
		data["settings"] = map[string]interface{}{}
	}
	s.lastLoadOk = true
	return data, true
}

// Save：临时文件 + rename 原子替换
func (s *Store) Save(data map[string]interface{}) bool {
	s.ensureDirs()
	if _, ok := data["todos"].([]interface{}); !ok {
		data["todos"] = []interface{}{}
	}
	if _, ok := data["settings"].(map[string]interface{}); !ok {
		data["settings"] = map[string]interface{}{}
	}
	data["version"] = 1
	data["savedAt"] = time.Now().UnixMilli()
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		appLogf("store: 序列化失败 %v", err)
		return false
	}
	if err := atomicWrite(s.tmpFile, s.dataFile, payload); err != nil {
		appLogf("store: 保存失败 %v", err)
		return false
	}
	return true
}

func atomicWrite(tmp, final string, payload []byte) error {
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// SaveWindowState / LoadWindowState：窗口几何独立文件
func (s *Store) SaveWindowState(state map[string]interface{}) {
	s.ensureDirs()
	state["savedAt"] = time.Now().UnixMilli()
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	if err := atomicWrite(s.windowTmp, s.windowFile, payload); err != nil {
		appLogf("store: 窗口状态保存失败 %v", err)
	}
}

func (s *Store) LoadWindowState() map[string]interface{} {
	raw, err := os.ReadFile(s.windowFile)
	if err != nil {
		return map[string]interface{}{}
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[string]interface{}{}
	}
	return m
}

/* ---------------------------------------------------------------- 图片 */

var safeExtRe = regexp.MustCompile(`(?i)^\.(png|jpg|jpeg|gif|bmp|webp|avif|svg|tif|tiff)$`)

func (s *Store) mediaPath(name string) string { return filepath.Join(s.mediaDir, name) }

// ImportImage：把外部图片复制进媒体目录（自检灌图用）
func (s *Store) ImportImage(srcPath string) (string, error) {
	s.ensureDirs()
	ext := strings.ToLower(filepath.Ext(srcPath))
	if ext == "" || !safeExtRe.MatchString(ext) {
		ext = ".png"
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%d_%s%s", time.Now().UnixMilli(), randToken(), ext)
	dest := s.mediaPath(name)
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

// mediaImageSize：解码媒体目录内图片的真实像素尺寸（自检断言用；不写死常量，
// 避免测试图片更换后断言悄然失效——历史教训：lightbox 断言曾硬编码 900x560）。
func (s *Store) mediaImageSize(name string) (int, int, error) {
	f, err := os.Open(s.mediaPath(name))
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

var dataUrlRe = regexp.MustCompile(`(?i)^data:(image/[a-z0-9.+-]+);base64,(.+)$`)

var dataUrlExt = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/jpg": ".jpg", "image/gif": ".gif",
	"image/bmp": ".bmp", "image/webp": ".webp", "image/avif": ".avif", "image/svg+xml": ".svg",
}

// ImportDataUrl：剪贴板 / 拖放的 data URL 落盘（渲染层拿到的是 File 对象，无真实路径）
func (s *Store) ImportDataUrl(dataUrl string) (string, error) {
	s.ensureDirs()
	m := dataUrlRe.FindStringSubmatch(dataUrl)
	if m == nil {
		return "", errors.New("不是合法的图片 data URL")
	}
	sub := strings.ToLower(m[1])
	ext, ok := dataUrlExt[sub]
	if !ok {
		ext = ".png"
	}
	buf, err := base64.StdEncoding.DecodeString(m[2])
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%d_%s%s", time.Now().UnixMilli(), randToken(), ext)
	dest := s.mediaPath(name)
	if err := os.WriteFile(dest, buf, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func (s *Store) RemoveImage(name string) bool {
	// 只允许纯文件名，防路径穿越
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	if err := os.Remove(s.mediaPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		appLogf("store: 删除图片失败 %v", err)
		return false
	}
	return true
}

type gcResult struct {
	Removed      int `json:"removed"`
	SkippedYoung int `json:"skippedYoung"`
}

// GcImages：清掉未被任何待办引用的图片。
// 护栏：只清理 mtime 早于 minAge 的文件（默认 7 天）——
// "加载失败→引用集为空→删光用户图片"的场景由调用方结合 lastLoadOk 拦截。
func (s *Store) GcImages(referenced []string, minAge time.Duration) gcResult {
	set := map[string]bool{}
	for _, r := range referenced {
		set[r] = true
	}
	res := gcResult{}
	entries, err := os.ReadDir(s.mediaDir)
	if err != nil {
		return res
	}
	cutoff := time.Now().Add(-minAge)
	for _, e := range entries {
		if e.IsDir() || set[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().After(cutoff) {
			res.SkippedYoung++
			continue
		}
		if os.Remove(s.mediaPath(e.Name())) == nil {
			res.Removed++
		}
	}
	return res
}

func (s *Store) Info() map[string]interface{} {
	return map[string]interface{}{
		"baseDir":    s.baseDir,
		"mediaDir":   s.mediaDir,
		"dataFile":   s.dataFile,
		"windowFile": s.windowFile,
		"portable":   s.portable,
	}
}
