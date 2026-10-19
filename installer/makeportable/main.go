//go:build windows

// makeportable —— 便携版 zip 打包工具（T5，纯打包零代码改动）
//
// 职责：
//   1. 从既有产物组装 staging（不重新构建任何东西）：
//        portable-staging\待办事项优先级提示窗-便携版-1.0beta\
//          待办事项优先级提示窗.exe  ← 复制 beta 产物（字节不变，改名）
//          assets\…                  ← 复制 app-go assets 全量
//          data\说明.txt             ← UTF-8 BOM 占位（便携模式开关）
//          使用说明.txt              ← UTF-8 BOM
//   2. archive/zip 打包（entry 顺序排序 + modtime 固定 → 可复现；非 ASCII 名自动置 UTF-8 标志位）
//   3. 输出 zip sha256/大小/条目数
//
// 用法：go run ./makeportable [-app <beta exe>] [-assets <目录>] [-out <zip>]
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	bom        = []byte{0xEF, 0xBB, 0xBF}
	betaName   = "待办事项优先级提示窗-便携版-1.0beta"
	mainExeName = "待办事项优先级提示窗.exe"
	stagingDir = "portable-staging"
)

const usageTxt = `==========================================
 待办事项优先级提示窗 · 便携版 1.0beta
==========================================

【使用】解压即用，无需安装
  1. 把「待办事项优先级提示窗-便携版-1.0beta」整个文件夹解压到任意位置
     （建议避开 Program Files 等可能有写入限制的系统位置）
  2. 双击「待办事项优先级提示窗.exe」即可运行

【数据】数据跟着文件夹走
  · 你的待办与图片数据保存在本文件夹的 data\ 目录中
  · 拷贝、移动、备份整个文件夹 = 数据随身迁移
  · 删除文件夹会连同数据一起删除，请先备份

【说明】
  · 与安装版相互独立、可共存：本便携版不向系统用户目录写入数据
  · 首次运行若出现 SmartScreen「未知发布者」提示，属预期（程序未做数字签名）：
    点击「更多信息」→「仍要运行」即可
  · 系统要求：Windows 10 / 11（64 位）；界面基于系统 WebView2 运行时
    （Windows 11 及较新的 Windows 10 已内置）

—— 待办事项优先级提示窗 1.0beta · 便携版
`

const dataReadmeTxt = `本目录存放你的待办数据（todos.json 与图片）。
请保留本目录（勿删、勿改名）——它是便携模式的标志：
删除后程序将改为把数据存到系统用户目录。
`

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "FAIL:", err)
	os.Exit(1)
}

func main() {
	app := flag.String("app", `..\app-go\dist\build-beta-out.exe`, "1.0beta 主程序产物路径（字节不变改名）")
	assets := flag.String("assets", `..\app-go\dist\assets`, "assets 全量目录")
	out := flag.String("out", `dist\待办事项优先级提示窗-便携版-1.0beta.zip`, "输出 zip 路径")
	flag.Parse()

	appBytes, err := os.ReadFile(*app)
	if err != nil {
		fatal(fmt.Errorf("读主程序 %s: %w", *app, err))
	}
	sum := sha256.Sum256(appBytes)
	fmt.Printf("main exe: %s (%d B) sha256=%s\n", *app, len(appBytes), hex.EncodeToString(sum[:]))

	// staging
	stg, err := filepath.Abs(stagingDir)
	if err != nil {
		fatal(err)
	}
	if err = os.RemoveAll(stg); err != nil {
		fatal(err)
	}
	root := filepath.Join(stg, betaName)
	if err = os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, mainExeName), appBytes, 0o644); err != nil {
		fatal(err)
	}
	if err = copyTree(*assets, filepath.Join(root, "assets")); err != nil {
		fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "使用说明.txt"), append(bom, usageTxt...), 0o644); err != nil {
		fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "data", "说明.txt"), append(bom, dataReadmeTxt...), 0o644); err != nil {
		fatal(err)
	}
	fmt.Println("staging:", root)

	// zip（确定性）
	if err = os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fatal(err)
	}
	zo, err := os.Create(*out)
	if err != nil {
		fatal(err)
	}
	defer zo.Close()
	zw := zip.NewWriter(zo)

	var entries []string
	err = filepath.WalkDir(stg, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			entries = append(entries, p)
		}
		return nil
	})
	if err != nil {
		fatal(err)
	}
	sort.Strings(entries)

	relOf := func(p string) string {
		// 保留 betaName 一级目录层（任务书 §二 zip 结构固定：解压出一个整体文件夹）
		return strings.TrimPrefix(filepath.ToSlash(p), filepath.ToSlash(stg)+"/")
	}
	seenDirs := map[string]bool{}
	ensureDirs := func(rel string) error {
		clean := rel
		for i := 0; i < len(clean) && i >= 0; i++ {
			if clean[i] != '/' {
				continue
			}
			dir := clean[:i+1]
			if seenDirs[dir] {
				continue
			}
			seenDirs[dir] = true
			zh := &zip.FileHeader{Name: dir, Modified: modtime()}
			zh.SetMode(fs.ModeDir | 0o755)
			if _, err = zw.CreateHeader(zh); err != nil {
				return err
			}
		}
		return nil
	}
	for _, p := range entries {
		rel := relOf(p)
		if err = ensureDirs(rel); err != nil {
			fatal(err)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			fatal(err)
		}
		fh := &zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: modtime()}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			fatal(err)
		}
		if _, err = io.Copy(w, bytes.NewReader(data)); err != nil {
			fatal(err)
		}
	}
	if err = zw.Close(); err != nil {
		fatal(err)
	}
	if err = zo.Sync(); err != nil {
		fatal(err)
	}

	zi, err := os.Stat(*out)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("zip: %s (%d B) sha256=%s\n", *out, zi.Size(), sha256SumFile(*out))
	fmt.Println("file entries:", len(entries))
}

func modtime() time.Time {
	return time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
}

func sha256SumFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "ERR " + err.Error()
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "ERR " + err.Error()
	}
	return hex.EncodeToString(h.Sum(nil))
}

func copyTree(src, dst string) error {
	return fs.WalkDir(os.DirFS(src), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, p)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(filepath.Join(src, p), target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
