//go:build windows

// gdi.go —— GDI 抓屏取证（与 toolchain/tools/shot 同款实现）。
// 用屏幕 DC 的 BitBlt 拿 DWM 合成后的最终画面：
// 若窗口不透明，卡片区域只会是一块实色；能透出下层内容即证明透明真实生效。
package main

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"unsafe"
)

var (
	pGetDC          = user32.NewProc("GetDC")
	pReleaseDC      = user32.NewProc("ReleaseDC")
	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pBitBlt             = gdi32.NewProc("BitBlt")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
	pDeleteDC           = gdi32.NewProc("DeleteDC")
)

const (
	SRCCOPY        = 0x00CC0020
	DIB_RGB_COLORS = 0
	BI_RGB         = 0
)

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type bitmapInfo struct {
	bmiHeader bitmapInfoHeader
	bmiColors [1]uint32
}

// captureRect：抓屏幕区域（物理像素），存成 PNG
func captureRect(outPath string, x, y, w, h int32) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("captureRect: 尺寸非法 %dx%d", w, h)
	}
	hdcScreen, _, _ := pGetDC.Call(0)
	if hdcScreen == 0 {
		return fmt.Errorf("GetDC 失败")
	}
	defer pReleaseDC.Call(0, hdcScreen)

	hdcMem, _, _ := pCreateCompatibleDC.Call(hdcScreen)
	if hdcMem == 0 {
		return fmt.Errorf("CreateCompatibleDC 失败")
	}
	defer pDeleteDC.Call(hdcMem)

	bmi := bitmapInfo{}
	bmi.bmiHeader.biSize = uint32(unsafe.Sizeof(bitmapInfoHeader{}))
	bmi.bmiHeader.biWidth = w
	bmi.bmiHeader.biHeight = -h // 自上而下
	bmi.bmiHeader.biPlanes = 1
	bmi.bmiHeader.biBitCount = 32
	bmi.bmiHeader.biCompression = BI_RGB

	var bits unsafe.Pointer
	hbm, _, _ := pCreateDIBSection.Call(
		hdcScreen, uintptr(unsafe.Pointer(&bmi)), DIB_RGB_COLORS,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		return fmt.Errorf("CreateDIBSection 失败")
	}
	defer pDeleteObject.Call(hbm)

	old, _, _ := pSelectObject.Call(hdcMem, hbm)
	defer pSelectObject.Call(hdcMem, old)

	if r, _, _ := pBitBlt.Call(hdcMem, 0, 0, uintptr(w), uintptr(h),
		hdcScreen, uintptr(x), uintptr(y), SRCCOPY); r == 0 {
		return fmt.Errorf("BitBlt 失败")
	}

	src := unsafe.Slice((*byte)(bits), int(w)*int(h)*4)
	img := image.NewRGBA(image.Rect(0, 0, int(w), int(h)))
	for i := 0; i < int(w)*int(h); i++ {
		img.Pix[i*4+0] = src[i*4+2] // R
		img.Pix[i*4+1] = src[i*4+1] // G
		img.Pix[i*4+2] = src[i*4+0] // B
		img.Pix[i*4+3] = 255
	}

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// captureWindowRegion：抓窗口所在屏幕区域（含周边桌面，可验证透明与叠放）
func captureWindowRegion(outPath string) error {
	r := windowRect()
	return captureRect(outPath, r.left, r.top, r.w(), r.h())
}

// captureRectPixels：抓屏幕区域并**返回像素缓冲**（不落盘），供断言内联判定。
// 返回 (pix, w, h, err)；pix 为 RGBA 行主序，长度 w*h*4。
func captureRectPixels(x, y, w, h int32) ([]byte, int, int, error) {
	if w <= 0 || h <= 0 {
		return nil, 0, 0, fmt.Errorf("captureRectPixels: 尺寸非法 %dx%d", w, h)
	}
	hdcScreen, _, _ := pGetDC.Call(0)
	if hdcScreen == 0 {
		return nil, 0, 0, fmt.Errorf("GetDC 失败")
	}
	defer pReleaseDC.Call(0, hdcScreen)

	hdcMem, _, _ := pCreateCompatibleDC.Call(hdcScreen)
	if hdcMem == 0 {
		return nil, 0, 0, fmt.Errorf("CreateCompatibleDC 失败")
	}
	defer pDeleteDC.Call(hdcMem)

	bmi := bitmapInfo{}
	bmi.bmiHeader.biSize = uint32(unsafe.Sizeof(bitmapInfoHeader{}))
	bmi.bmiHeader.biWidth = w
	bmi.bmiHeader.biHeight = -h
	bmi.bmiHeader.biPlanes = 1
	bmi.bmiHeader.biBitCount = 32
	bmi.bmiHeader.biCompression = BI_RGB

	var bits unsafe.Pointer
	hbm, _, _ := pCreateDIBSection.Call(
		hdcScreen, uintptr(unsafe.Pointer(&bmi)), DIB_RGB_COLORS,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		return nil, 0, 0, fmt.Errorf("CreateDIBSection 失败")
	}
	defer pDeleteObject.Call(hbm)

	old, _, _ := pSelectObject.Call(hdcMem, hbm)
	defer pSelectObject.Call(hdcMem, old)

	if r, _, _ := pBitBlt.Call(hdcMem, 0, 0, uintptr(w), uintptr(h),
		hdcScreen, uintptr(x), uintptr(y), SRCCOPY); r == 0 {
		return nil, 0, 0, fmt.Errorf("BitBlt 失败")
	}

	src := unsafe.Slice((*byte)(bits), int(w)*int(h)*4)
	out := make([]byte, int(w)*int(h)*4)
	copy(out, src) // 拷出后再解码（bits 随 hbm 释放失效）
	return out, int(w), int(h), nil
}

// rgbAt：从 captureRectPixels 的 BGRX 缓冲取 (r,g,b)
func rgbAt(pix []byte, w, x, y int) (int, int, int) {
	o := (y*w + x) * 4
	return int(pix[o+2]), int(pix[o+1]), int(pix[o])
}

// captureFullScreen：抓主屏全屏
func captureFullScreen(outPath string) error {
	cx, _, _ := pGetSystemMetrics.Call(SM_CXSCREEN)
	cy, _, _ := pGetSystemMetrics.Call(SM_CYSCREEN)
	return captureRect(outPath, 0, 0, int32(cx), int32(cy))
}
