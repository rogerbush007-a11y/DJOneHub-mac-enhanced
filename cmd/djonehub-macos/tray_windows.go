//go:build windows

package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"log"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmDestroy        = 0x0002
	wmClose          = 0x0010
	wmCommand        = 0x0111
	wmLButtonUp      = 0x0202
	wmLButtonDblClk  = 0x0203
	wmRButtonUp      = 0x0205
	wmContextMenu    = 0x007B
	wmApp            = 0x8000
	wmTrayCallback   = wmApp + 1
	nimAdd           = 0x00000000
	nimDelete        = 0x00000002
	nifMessage       = 0x00000001
	nifIcon          = 0x00000002
	nifTip           = 0x00000004
	mfString         = 0x00000000
	mfSeparator      = 0x00000800
	tpmRightButton   = 0x0002
	tpmBottomAlign   = 0x0020
	tpmReturnCmd     = 0x0100
	idiApplication   = 32512
	lrDefaultColor   = 0x00000000
	iconVersion      = 0x00030000
	menuOpenUIID     = 1001
	menuOpenLogsID   = 1002
	menuExitID       = 1003
	trayIconID       = 1
	trayClassName    = "DJOneHubTrayWindowClass"
	trayTooltipTitle = "DJOneHub"
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	shell32                 = windows.NewLazySystemDLL("shell32.dll")
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procUnregisterClassW    = user32.NewProc("UnregisterClassW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procLoadIconW           = user32.NewProc("LoadIconW")
	procCreateIconFromResEx = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procEnumWindows         = user32.NewProc("EnumWindows")
	procGetClassNameW       = user32.NewProc("GetClassNameW")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
	procShellNotifyIconW    = shell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW       = shell32.NewProc("ShellExecuteW")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type point struct {
	x int32
	y int32
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type notifyIconDataW struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

type windowsTrayState struct {
	hwnd       uintptr
	hIcon      uintptr
	ownedIcon  bool
	uiURL      string
	cancel     context.CancelFunc
	removeOnce sync.Once
}

func startWindowsTray(ctx context.Context, cancel context.CancelFunc, uiURL string) func() {
	ready := make(chan uintptr, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		hInstance, _, _ := procGetModuleHandleW.Call(0)
		classNamePtr, _ := windows.UTF16PtrFromString(trayClassName)
		windowTitlePtr, _ := windows.UTF16PtrFromString(trayTooltipTitle)

		state := &windowsTrayState{
			uiURL:  uiURL,
			cancel: cancel,
		}

		wndProc := syscall.NewCallback(func(hwnd uintptr, uMsg uint32, wParam, lParam uintptr) uintptr {
			return state.handleMessage(hwnd, uMsg, wParam, lParam)
		})

		wc := wndClassExW{
			cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
			lpfnWndProc:   wndProc,
			hInstance:     hInstance,
			lpszClassName: classNamePtr,
		}
		atom, _, regErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		if atom == 0 {
			log.Printf("windows tray register class failed: %v", regErr)
			ready <- 0
			return
		}
		defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(classNamePtr)), hInstance)

		hwnd, _, createErr := procCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(classNamePtr)),
			uintptr(unsafe.Pointer(windowTitlePtr)),
			0,
			0, 0, 0, 0,
			0, 0, hInstance, 0,
		)
		if hwnd == 0 {
			log.Printf("windows tray create window failed: %v", createErr)
			ready <- 0
			return
		}
		state.hwnd = hwnd

		hIcon, owned := createTrayIcon()
		state.hIcon = hIcon
		state.ownedIcon = owned
		defer func() {
			state.removeTrayIcon()
			if state.ownedIcon && state.hIcon != 0 {
				_, _, _ = procDestroyIcon.Call(state.hIcon)
			}
		}()

		if !state.addTrayIcon() {
			log.Printf("windows tray add icon failed")
		}
		ready <- hwnd

		var m msg
		for {
			ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(ret) <= 0 {
				break
			}
			_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()

	hwnd := <-ready
	stopOnce := sync.Once{}
	stop := func() {
		stopOnce.Do(func() {
			closePlatformUIWindows()
			if hwnd != 0 {
				_, _, _ = procPostMessageW.Call(hwnd, wmClose, 0, 0)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		})
	}

	go func() {
		<-ctx.Done()
		stop()
	}()

	return stop
}

func (s *windowsTrayState) handleMessage(hwnd uintptr, uMsg uint32, wParam, lParam uintptr) uintptr {
	switch uMsg {
	case wmTrayCallback:
		switch uint32(lParam) {
		case wmLButtonUp, wmLButtonDblClk:
			openPlatformUI(s.uiURL)
		case wmRButtonUp, wmContextMenu:
			s.showContextMenu(hwnd)
		}
		return 0
	case wmCommand:
		switch uint16(wParam & 0xffff) {
		case menuOpenUIID:
			openPlatformUI(s.uiURL)
		case menuOpenLogsID:
			openWindowsLogDir()
		case menuExitID:
			s.removeTrayIcon()
			closePlatformUIWindows()
			if s.cancel != nil {
				s.cancel()
			}
			_, _, _ = procDestroyWindow.Call(hwnd)
		}
		return 0
	case wmClose:
		s.removeTrayIcon()
		_, _, _ = procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		s.removeTrayIcon()
		_, _, _ = procPostQuitMessage.Call(0)
		return 0
	default:
		ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(uMsg), wParam, lParam)
		return ret
	}
}

func (s *windowsTrayState) addTrayIcon() bool {
	nid := notifyIconDataW{
		cbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:             s.hwnd,
		uID:              trayIconID,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayCallback,
		hIcon:            s.hIcon,
	}
	copyUTF16(nid.szTip[:], trayTooltipTitle)
	ret, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	return ret != 0
}

func (s *windowsTrayState) removeTrayIcon() {
	s.removeOnce.Do(func() {
		if s.hwnd == 0 {
			return
		}
		nid := notifyIconDataW{
			cbSize: uint32(unsafe.Sizeof(notifyIconDataW{})),
			hWnd:   s.hwnd,
			uID:    trayIconID,
		}
		_, _, _ = procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	})
}

func (s *windowsTrayState) showContextMenu(hwnd uintptr) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	openLabel, _ := windows.UTF16PtrFromString("打开控制面板")
	logsLabel, _ := windows.UTF16PtrFromString("查看运行日志")
	exitLabel, _ := windows.UTF16PtrFromString("退出 DJOneHub")

	_, _, _ = procAppendMenuW.Call(menu, mfString, menuOpenUIID, uintptr(unsafe.Pointer(openLabel)))
	_, _, _ = procAppendMenuW.Call(menu, mfString, menuOpenLogsID, uintptr(unsafe.Pointer(logsLabel)))
	_, _, _ = procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	_, _, _ = procAppendMenuW.Call(menu, mfString, menuExitID, uintptr(unsafe.Pointer(exitLabel)))

	var pt point
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	_, _, _ = procSetForegroundWindow.Call(hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(
		menu,
		tpmRightButton|tpmBottomAlign|tpmReturnCmd,
		uintptr(pt.x),
		uintptr(pt.y),
		0,
		hwnd,
		0,
	)
	if cmd != 0 {
		_, _, _ = procPostMessageW.Call(hwnd, wmCommand, cmd, 0)
	}
}

func copyUTF16(dst []uint16, text string) {
	u16, err := windows.UTF16FromString(text)
	if err != nil || len(dst) == 0 {
		return
	}
	n := len(u16)
	if n > len(dst) {
		n = len(dst)
	}
	copy(dst[:n], u16[:n])
	dst[len(dst)-1] = 0
}

func createTrayIcon() (uintptr, bool) {
	if pngBytes := buildTrayPNG(); len(pngBytes) > 0 {
		hIcon, _, _ := procCreateIconFromResEx.Call(
			uintptr(unsafe.Pointer(&pngBytes[0])),
			uintptr(len(pngBytes)),
			1,
			iconVersion,
			32,
			32,
			lrDefaultColor,
		)
		if hIcon != 0 {
			return hIcon, true
		}
	}
	hIcon, _, _ := procLoadIconW.Call(0, idiApplication)
	return hIcon, false
}

func buildTrayPNG() []byte {
	const size = 32
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	bg := color.NRGBA{R: 15, G: 118, B: 230, A: 255}
	fg := color.NRGBA{R: 255, G: 255, B: 255, A: 255}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := x - size/2
			dy := y - size/2
			// Draw a clean rounded square / badge background.
			ax := dx
			if ax < 0 {
				ax = -ax
			}
			ay := dy
			if ay < 0 {
				ay = -ay
			}
			if ax <= 13 && ay <= 13 && (ax <= 10 || ay <= 10 || (ax-10)*(ax-10)+(ay-10)*(ay-10) <= 9) {
				img.SetNRGBA(x, y, bg)
			}
		}
	}

	// Draw 3 signal bars inside the badge.
	bars := [][4]int{
		{9, 18, 12, 23},
		{14, 14, 17, 23},
		{19, 9, 22, 23},
	}
	for _, b := range bars {
		for y := b[1]; y <= b[3]; y++ {
			for x := b[0]; x <= b[2]; x++ {
				img.SetNRGBA(x, y, fg)
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

func shellOpenPath(target, workDir string) bool {
	verbPtr, _ := windows.UTF16PtrFromString("open")
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return false
	}
	var dirPtr *uint16
	if strings.TrimSpace(workDir) != "" {
		dirPtr, _ = windows.UTF16PtrFromString(workDir)
	}
	const swShowNormal = 1
	ret, _, _ := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verbPtr)),
		uintptr(unsafe.Pointer(targetPtr)),
		0,
		uintptr(unsafe.Pointer(dirPtr)),
		swShowNormal,
	)
	return ret > 32
}

func closePlatformUIWindows() {
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		visible, _, _ := procIsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		var classBuf [128]uint16
		n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&classBuf[0])), uintptr(len(classBuf)))
		if n == 0 {
			return 1
		}
		className := windows.UTF16ToString(classBuf[:n])
		if className != "Chrome_WidgetWin_1" {
			return 1
		}
		var titleBuf [256]uint16
		tn, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), uintptr(len(titleBuf)))
		if tn == 0 {
			return 1
		}
		title := windows.UTF16ToString(titleBuf[:tn])
		// Match the Edge --app window title without closing regular multi-tab Edge windows.
		if strings.HasPrefix(title, "DJOneHub") && !strings.Contains(title, "Microsoft") {
			_, _, _ = procPostMessageW.Call(hwnd, wmClose, 0, 0)
		}
		return 1
	})
	_, _, _ = procEnumWindows.Call(cb, 0)
}
