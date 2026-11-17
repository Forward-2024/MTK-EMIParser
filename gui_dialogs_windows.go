package main

// 菜单栏 + 需要密码的“修改内置数据库”对话框 + 关于框。
// 全部用 Win32 原生控件（EDIT / BUTTON / STATIC），不引第三方库。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	idMenuOpen     = 2001
	idMenuClear    = 2002
	idMenuExit     = 2003
	idMenuDbInfo   = 2011
	idMenuDbEdit   = 2012
	idMenuDbReload = 2013
	idMenuDbWhere  = 2014
	idMenuAbout    = 2021

	idDlgOK     = 4001
	idDlgCancel = 4002
	idDlgReset  = 4003

	dlgClass = "MTKEmiParserDialogWindow"

	esAutoVScroll = 0x0040
	esWantReturn  = 0x1000
	esPassword    = 0x0020
	wsPopup       = 0x80000000
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsExDlgModal  = 0x00010000

	mfString    = 0x0000
	mfPopup     = 0x0010
	mfSeparator = 0x0800

	wmKeyDown   = 0x0100
	wmGetText   = 0x000D
	wmTextLen   = 0x000E
	wmTimer     = 0x0113
	gwlpWndProc = ^uintptr(3) // -4
)

var (
	procCreateMenu      = user32.NewProc("CreateMenu")
	procCreatePopupMenu = user32.NewProc("CreatePopupMenu")
	procAppendMenuW     = user32.NewProc("AppendMenuW")
	procSetMenu         = user32.NewProc("SetMenu")
	procDrawMenuBar     = user32.NewProc("DrawMenuBar")
	procGetMessageW     = user32.NewProc("GetMessageW")
	procEnableWindow    = user32.NewProc("EnableWindow")
	procGetWindowRect   = user32.NewProc("GetWindowRect")
	procSetWindowPos    = user32.NewProc("SetWindowPos")
	procSetTimer        = user32.NewProc("SetTimer")
	procKillTimer       = user32.NewProc("KillTimer")

	procShellExecuteW = shell32.NewProc("ShellExecuteW")
)

// ---------------------------------------------------------------- 菜单

func menuItem(h uintptr, id int, label string) {
	procAppendMenuW.Call(h, mfString, uintptr(id), uintptr(unsafe.Pointer(u16ptr(label))))
}

func buildMenu(hwnd uintptr) {
	hMenu, _, _ := procCreateMenu.Call()
	if hMenu == 0 {
		return
	}

	hFile, _, _ := procCreatePopupMenu.Call()
	menuItem(hFile, idMenuOpen, "打开文件…")
	menuItem(hFile, idMenuClear, "清空列表")
	procAppendMenuW.Call(hFile, mfSeparator, 0, 0)
	menuItem(hFile, idMenuExit, "退出")
	procAppendMenuW.Call(hMenu, mfPopup, hFile, uintptr(unsafe.Pointer(u16ptr("文件(&F)"))))

	hDB, _, _ := procCreatePopupMenu.Call()
	menuItem(hDB, idMenuDbInfo, "数据库信息…")
	menuItem(hDB, idMenuDbEdit, "修改内置数据库…（需要密码）")
	procAppendMenuW.Call(hDB, mfSeparator, 0, 0)
	menuItem(hDB, idMenuDbReload, "重新载入数据库")
	menuItem(hDB, idMenuDbWhere, "打开数据库所在目录")
	procAppendMenuW.Call(hMenu, mfPopup, hDB, uintptr(unsafe.Pointer(u16ptr("数据库(&D)"))))

	hHelp, _, _ := procCreatePopupMenu.Call()
	menuItem(hHelp, idMenuAbout, "关于 / 版本 / 校验码")
	procAppendMenuW.Call(hMenu, mfPopup, hHelp, uintptr(unsafe.Pointer(u16ptr("帮助(&H)"))))

	procSetMenu.Call(hwnd, hMenu)
	procDrawMenuBar.Call(hwnd)
}

// ---------------------------------------------------------------- 标题

func windowTitle() string {
	ver := DB().version
	if ver == "" {
		ver = "未知"
	}
	return fmt.Sprintf("%s  ·  v%s  ·  数据库 %s", appTitle, appVersion, ver)
}

func updateTitle(hwnd uintptr) {
	procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(u16ptr(windowTitle()))))
}

// ---------------------------------------------------------------- 通用对话框

type dlgT struct {
	hwnd    uintptr
	hEdit   uintptr
	hOk     uintptr
	hCancel uintptr
	hReset  uintptr
	oldProc uintptr

	title  string
	prompt string
	multi  bool
	pass   bool
	text   string
	okText string

	ok   bool
	done bool

	// 只给自检用：定时后自动填入并按下“确定”
	auto   string
	autoMs uint32
}

var curDlg *dlgT

func dlgWndProc(hwnd uintptr, m uint32, wParam, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			writeCrashLog(fmt.Sprintf("panic in dlgWndProc msg=0x%04X: %v", m, r))
			ret = 0
		}
	}()
	d := curDlg
	if d == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(m), wParam, lParam)
		return r
	}
	switch m {
	case wmCreate:
		d.hwnd = hwnd
		d.build()
		return 0
	case wmTimer:
		if d.autoMs != 0 {
			procKillTimer.Call(hwnd, 1)
			d.autoMs = 0
			if d.hEdit != 0 {
				setText(d.hEdit, dlgBody(d.auto, d.multi))
			}
			procPostMessageW.Call(hwnd, wmCommand, uintptr(idDlgOK), 0)
		}
		return 0
	case wmCommand:
		switch uint32(wParam & 0xffff) {
		case idDlgOK:
			if d.hEdit != 0 {
				d.text = dlgReadBody(getText(d.hEdit), d.multi)
			}
			d.ok = true
			procDestroyWindow.Call(hwnd)
			return 0
		case idDlgCancel:
			procDestroyWindow.Call(hwnd)
			return 0
		case idDlgReset:
			if d.hEdit != 0 {
				if raw, err := gunzip(pnmDBGz); err == nil {
					setText(d.hEdit, dlgBody(string(raw), true))
				}
			}
			return 0
		}
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		d.done = true
		return 0
	case wmCtlColorStatic, wmCtlColorBtn:
		br, _, _ := procCreateSolidBrush.Call(clrCard)
		procSetBkColor.Call(wParam, clrCard)
		return br
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(m), wParam, lParam)
	return r
}

func (d *dlgT) build() {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	var rc rectT
	procGetClientRect.Call(d.hwnd, uintptr(unsafe.Pointer(&rc)))
	w, h := int32(rc.right), int32(rc.bottom)
	m := S(14)
	y := m

	if d.prompt != "" {
		lh := S(22)
		if d.multi {
			lh = S(44)
		}
		hs, _, _ := procCreateWindowExW.Call(0,
			uintptr(unsafe.Pointer(u16ptr("STATIC"))),
			uintptr(unsafe.Pointer(u16ptr(d.prompt))),
			uintptr(wsChild|wsVisible),
			uintptr(m), uintptr(y), uintptr(w-2*m), uintptr(lh),
			d.hwnd, 0, hInst, 0)
		if hs != 0 && currentGUI != nil {
			procSendMessageW.Call(hs, wmSetFont, currentGUI.fontUI, 1)
		}
		y += lh + S(4)
	}

	bw, bh := S(96), S(30)
	by := h - m - bh
	editH := by - S(10) - y
	if editH < S(40) {
		editH = S(40)
	}

	style := uint32(wsChild | wsVisible | wsTabStop | wsBorder | esNoHideSel)
	if d.multi {
		style |= esMultiline | esWantReturn | wsVScroll | esAutoVScroll
	}
	if d.pass {
		style |= esPassword
	}
	he, _, _ := procCreateWindowExW.Call(wsExClientEdge,
		uintptr(unsafe.Pointer(u16ptr("EDIT"))), 0,
		uintptr(style),
		uintptr(m), uintptr(y), uintptr(w-2*m), uintptr(editH),
		d.hwnd, 0, hInst, 0)
	d.hEdit = he
	if he != 0 {
		font := uintptr(0)
		if currentGUI != nil {
			font = currentGUI.fontUI
			if d.multi {
				font = currentGUI.fontMono
			}
		}
		if font != 0 {
			procSendMessageW.Call(he, wmSetFont, font, 1)
		}
		setText(he, dlgBody(d.text, d.multi))
		// 回车/ESC 处理（单行框回车 = 确定，ESC = 取消）
		if old, _, _ := procSetWindowLongPtrW.Call(he, gwlpWndProc, dlgEditProcAddr); old != 0 {
			d.oldProc = old
		}
	}

	x := w - m - bw
	mk := func(label string, id int) uintptr {
		hb, _, _ := procCreateWindowExW.Call(0,
			uintptr(unsafe.Pointer(u16ptr("BUTTON"))),
			uintptr(unsafe.Pointer(u16ptr(label))),
			uintptr(wsChild|wsVisible|wsTabStop),
			uintptr(x), uintptr(by), uintptr(bw), uintptr(bh),
			d.hwnd, uintptr(id), hInst, 0)
		if hb != 0 && currentGUI != nil {
			procSendMessageW.Call(hb, wmSetFont, currentGUI.fontUI, 1)
		}
		x -= bw + S(8)
		return hb
	}
	okLabel := d.okText
	if okLabel == "" {
		okLabel = "确定"
	}
	d.hOk = mk(okLabel, idDlgOK)
	d.hCancel = mk("取消", idDlgCancel)
	if d.multi {
		d.hReset = mk("还原内置", idDlgReset)
	}
}

func dlgEditProc(hwnd uintptr, m uint32, wParam, lParam uintptr) (ret uintptr) {
	d := curDlg
	if d != nil && m == wmKeyDown {
		switch wParam {
		case 0x0D: // Enter
			if !d.multi {
				procPostMessageW.Call(d.hwnd, wmCommand, uintptr(idDlgOK), 0)
				return 0
			}
		case 0x1B: // Esc
			procPostMessageW.Call(d.hwnd, wmCommand, uintptr(idDlgCancel), 0)
			return 0
		}
	}
	old := uintptr(0)
	if d != nil {
		old = d.oldProc
	}
	r, _, _ := procCallWindowProcW.Call(old, hwnd, uintptr(m), wParam, lParam)
	return r
}

var dlgProcAddr = syscall.NewCallback(dlgWndProc)
var dlgEditProcAddr = syscall.NewCallback(dlgEditProc)

var dlgRegistered bool

func showDialog(owner uintptr, d *dlgT) bool {
	if !dlgRegistered {
		hInst, _, _ := procGetModuleHandleW.Call(0)
		hCursor, _, _ := procLoadCursorW.Call(0, idcArrow)
		wc := wndClassEx{
			lpfnWndProc:   dlgProcAddr,
			hInstance:     hInst,
			hCursor:       hCursor,
			hbrBackground: 6,
			lpszClassName: u16ptr(dlgClass),
		}
		wc.cbSize = uint32(unsafe.Sizeof(wc))
		if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			return false
		}
		dlgRegistered = true
	}

	hInst, _, _ := procGetModuleHandleW.Call(0)

	// 尺寸单独给？这里统一在 build 之前算好
	cw, ch := int32(S(420)), int32(S(180))
	if d.multi {
		cw, ch = S(820), S(640)
	}

	// 居中到主窗口
	x, y := int32(S(200)), int32(S(160))
	if owner != 0 {
		var rc rectT
		if r, _, _ := procGetWindowRect.Call(owner, uintptr(unsafe.Pointer(&rc))); r != 0 {
			x = rc.left + (rc.right-rc.left-cw)/2
			y = rc.top + (rc.bottom-rc.top-ch)/2
		}
	}

	curDlg = d
	hwnd, _, _ := procCreateWindowExW.Call(wsExDlgModal,
		uintptr(unsafe.Pointer(u16ptr(dlgClass))),
		uintptr(unsafe.Pointer(u16ptr(d.title))),
		uintptr(wsPopup|wsCaption|wsSysMenu|wsVisible),
		uintptr(x), uintptr(y), uintptr(cw), uintptr(ch),
		owner, 0, hInst, 0)
	if hwnd == 0 {
		curDlg = nil
		return false
	}

	if owner != 0 {
		procEnableWindow.Call(owner, 0)
	}
	if d.autoMs != 0 {
		procSetTimer.Call(hwnd, 1, uintptr(d.autoMs), 0)
	}
	if d.hEdit != 0 {
		procSetFocus.Call(d.hEdit)
		if d.pass || !d.multi {
			procSendMessageW.Call(d.hEdit, 0x00B1, 0, ^uintptr(0)) // EM_SETSEL 全选
		}
	}

	var msg msgT
	for !d.done {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	if owner != 0 {
		procEnableWindow.Call(owner, 1)
		procSetFocus.Call(owner)
	}
	curDlg = nil
	return d.ok
}

func getText(h uintptr) string {
	n, _, _ := procSendMessageW.Call(h, wmTextLen, 0, 0)
	if int32(n) <= 0 {
		return ""
	}
	buf := make([]uint16, int(n)+2)
	procSendMessageW.Call(h, wmGetText, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	return syscall.UTF16ToString(buf)
}

func setText(h uintptr, s string) {
	procSetWindowTextW.Call(h, uintptr(unsafe.Pointer(u16ptr(s))))
}

// dlgBody：多行 EDIT 要 CRLF 才换行；单行框原样给
func dlgBody(s string, multi bool) string {
	if !multi {
		return s
	}
	return toCRLF(s)
}

// dlgReadBody：把 EDIT 里的 CRLF 还原成 LF 再落盘
func dlgReadBody(s string, multi bool) string {
	if !multi {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// askText 弹一个单行输入框（密码框就用 pass=true）
func askText(owner uintptr, title, prompt string, pass bool) (string, bool) {
	d := &dlgT{title: title, prompt: prompt, pass: pass}
	if !showDialog(owner, d) {
		return "", false
	}
	return d.text, true
}

// editText 弹一个多行编辑框
func editText(owner uintptr, title, prompt, text, okLabel string) (string, bool) {
	d := &dlgT{title: title, prompt: prompt, multi: true, text: text, okText: okLabel}
	if !showDialog(owner, d) {
		return "", false
	}
	return d.text, true
}

// ---------------------------------------------------------------- 数据库流程

const dbPassword = "forward"

func selfHash() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func dbSigLine() string {
	d := DB()
	switch {
	case d.err != "":
		return "读取失败：" + d.err
	case d.builtin:
		return "内置副本（没能写到磁盘，当前用 exe 内的数据）"
	case d.edited:
		return "SHA-256 " + d.sum + "…（本地文件被修改过，和内置副本不一致）"
	default:
		return "SHA-256 " + d.sum + "…（与内置副本一致）"
	}
}

// runDBTest 是 -dbtest 的自检：跑一遍密码框（错/对）和数据库编辑器（载入/保存），结果写文件。
func runDBTest(g *guiT, hwnd uintptr, path string) {
	var b strings.Builder
	d0 := DB()
	fmt.Fprintf(&b, "DB: %s\npath: %s\nrows=%d version=%s sum=%s edited=%v builtin=%v err=%q\n",
		DBStatus(), d0.path, len(d0.rows), d0.version, d0.sum, d0.edited, d0.builtin, d0.err)

	// 1. 密码框：先输错，再输对
	d := &dlgT{title: "修改内置数据库", prompt: "请输入密码（修改 PNM 型号库需要密码）：", pass: true,
		auto: "wrong", autoMs: 500}
	ok := showDialog(hwnd, d)
	fmt.Fprintf(&b, "pwd wrong: ok=%v text=%q accept=%v\n", ok, d.text, strings.TrimSpace(d.text) == dbPassword)

	d = &dlgT{title: "修改内置数据库", prompt: "请输入密码（修改 PNM 型号库需要密码）：", pass: true,
		auto: dbPassword, autoMs: 500}
	ok = showDialog(hwnd, d)
	fmt.Fprintf(&b, "pwd right: ok=%v text=%q accept=%v\n", ok, d.text, strings.TrimSpace(d.text) == dbPassword)

	// 2. 编辑器：内容能完整取出、能原样写回
	raw, _ := os.ReadFile(d0.path)
	d = &dlgT{title: "修改内置数据库 · " + d0.path, prompt: "自检：这段文字应该能完整取出",
		multi: true, text: string(raw), okText: "保存", auto: string(raw), autoMs: 4000}
	ok = showDialog(hwnd, d)
	fmt.Fprintf(&b, "editor: ok=%v len_in=%d len_out=%d same=%v\n", ok, len(raw), len(d.text), d.text == string(raw))

	// 3. 写回 + 重新载入
	if ok {
		werr := os.WriteFile(d0.path, []byte(d.text), 0o644)
		ReloadDB()
		fmt.Fprintf(&b, "write: err=%v reload: %s\n", werr, DBStatus())
	}

	_ = os.WriteFile(path, []byte(b.String()), 0o644)
	guiTrace("dbtest wrote %s", path)
}

func showAbout(owner uintptr) {
	exe := selfHash()
	if len(exe) > 32 {
		exe = exe[:32] + "…"
	}
	txt := fmt.Sprintf(
		"%s\n版本 %s\n\n%s\n文件位置：%s\n\n"+
			"数据库校验值：\n  %s\n\n"+
			"exe 校验值（SHA-256 前 32 位）：\n  %s\n"+
			"完整值见 exe 同目录的 MTKEmiParser.exe.sha256",
		appTitle, appVersion, DBStatus(), DB().path, dbSigLine(), exe)
	procMessageBoxW.Call(owner, uintptr(unsafe.Pointer(u16ptr(txt))),
		uintptr(unsafe.Pointer(u16ptr("关于 · MTK EMI 解析器"))), 0x40)
}

func updateDBAndRefresh(owner uintptr) {
	ReloadDB()
	if currentGUI != nil {
		g := currentGUI
		g.refreshInfo()
		g.refreshDetail()
		g.setStatus("数据库已重新载入 · " + DBStatus())
	}
	updateTitle(owner)
}

func (g *guiT) menuEditDB() {
	pw, ok := askText(g.hwnd, "修改内置数据库", "请输入密码（修改 PNM 型号库需要密码）：", true)
	if !ok {
		return
	}
	if strings.TrimSpace(pw) != dbPassword {
		procMessageBoxW.Call(g.hwnd,
			uintptr(unsafe.Pointer(u16ptr("密码不对，不能修改数据库。"))),
			uintptr(unsafe.Pointer(u16ptr("修改内置数据库"))), 0x30)
		g.setStatus("数据库修改：密码错误")
		return
	}

	d := DB()
	text := ""
	if raw, err := os.ReadFile(d.path); err == nil {
		text = string(raw)
	} else if raw, err := gunzip(pnmDBGz); err == nil {
		text = string(raw)
	}

	hint := "制表符分隔：# 开头是注释；PNM + DRAM_GB 相同的行会覆盖内置数据。\n" +
		"改完点「保存」，程序会立刻重新载入。"
	nt, ok := editText(g.hwnd, "修改内置数据库 · "+DB().path, hint, text, "保存")
	if !ok {
		g.setStatus("数据库修改：已取消")
		return
	}
	if err := os.WriteFile(d.path, []byte(nt), 0o644); err != nil {
		procMessageBoxW.Call(g.hwnd,
			uintptr(unsafe.Pointer(u16ptr("保存失败："+err.Error()))),
			uintptr(unsafe.Pointer(u16ptr("修改内置数据库"))), 0x10)
		return
	}
	updateDBAndRefresh(g.hwnd)
}

func (g *guiT) menuDbInfo() {
	d := DB()
	txt := fmt.Sprintf("%s\n\n文件：%s\n版本：%s\n条目：%d\n\n校验：%s\n\n"+
		"同一个 PNM 可能对应多种容量（例如海力士 2+32 / 3+32 / 4+32 的 CID 一样），\n"+
		"所以查表时用「PNM + EMI 里的 DRAM 容量」一起匹配；容量对不上会把候选全列出来。",
		DBStatus(), d.path, d.version, len(d.rows), dbSigLine())
	procMessageBoxW.Call(g.hwnd, uintptr(unsafe.Pointer(u16ptr(txt))),
		uintptr(unsafe.Pointer(u16ptr("数据库信息"))), 0x40)
}

func (g *guiT) menuDbWhere() {
	p := DB().path
	procShellExecuteW.Call(g.hwnd,
		uintptr(unsafe.Pointer(u16ptr("open"))),
		uintptr(unsafe.Pointer(u16ptr("explorer.exe"))),
		uintptr(unsafe.Pointer(u16ptr("/select,"+p))), 0, 1)
}
