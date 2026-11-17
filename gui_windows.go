//go:build windows

package main

// Win32 界面（纯 syscall，无 cgo、无第三方库）。
//
// 布局参照 keymap/lkpanel（三块式）：
//
//	┌ 标题 ──────────────────────────────────────────────┐
//	│ [选择文件…] [清空] [复制详情]                        │
//	├ 左上 当前文件 ─────┬ 右侧 详情 ─────────────────────┤
//	│ 目录 / 大小 / 汇总  │ 选中颗粒的全部字段            │
//	├ 左下 闪存列表 ─────┤                              │
//	│ [ ] 按时序排序      │                              │
//	└ 状态栏 ────────────┴──────────────────────────────┘
//
// 列表是自绘 LISTBOX（两行：PNM 一行、厂商/封装/速率一行），点一条右边换详情。

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// ---------------------------------------------------------------- 常量

const (
	guiClass = "MTKEmiParserMainWindow"
	appTitle = "MTK EMI 解析器 — preloader / boot1 闪存与 DRAM 配置"

	idBtnOpen  = 1001
	idBtnClear = 1002
	idBtnCopy  = 1003
	idChkSort  = 1004
	idInfo     = 1005
	idList     = 1006
	idDetail   = 1007

	wsOverlappedWindow = 0x00CF0000
	wsClipChildren     = 0x02000000
	wsVisible          = 0x10000000
	wsChild            = 0x40000000
	wsTabStop          = 0x00010000
	wsVScroll          = 0x00200000
	wsBorder           = 0x00800000
	wsExClientEdge     = 0x00000200

	esMultiline  = 0x0004
	esReadOnly   = 0x0800
	esNoHideSel  = 0x0100
	bsOwnerDraw  = 0x0000000B
	bsAutoCheck  = 0x00000003
	lbsNotify    = 0x0001
	lbsOwnerDraw = 0x0010
	lbsNoIntegrl = 0x0100
	lbsHasStr    = 0x0040

	wmCreate         = 0x0001
	wmDestroy        = 0x0002
	wmSize           = 0x0005
	wmPaint          = 0x000F
	wmClose          = 0x0010
	wmEraseBkgnd     = 0x0014
	wmSetFont        = 0x0030
	wmGetMinMax      = 0x0024
	wmMeasureItem    = 0x002C
	wmDrawItem       = 0x002B
	wmCommand        = 0x0111
	wmDropFiles      = 0x0233
	wmCtlColorBtn    = 0x0135
	wmCtlColorStatic = 0x0138
	wmSetRedraw      = 0x000B
	wmAppDone        = 0x8001
	wmQuit           = 0x0012

	emSetCharFormat = 0x0444
	emSetTextEx     = 0x0461
	emSetSel        = 0x00B1
	emSetRect       = 0x00B3
	emExLimitText   = 0x0435
	emScrollCaret   = 0x00B7
	emSetBkColor    = 0x0443
	emHideSelection = 0x0415
	emSetReadOnly   = 0x00CF
	scfAll          = 0x0004
	scfSelection    = 0x0001
	cfmBold         = 0x00000001
	cfmItalic       = 0x00000002
	cfmColor        = 0x40000000
	cfmFace         = 0x20000000
	cfmSize         = 0x80000000
	cfmWeight       = 0x00400000
	cfeBold         = 0x0001

	lbAddString    = 0x0180
	lbResetContent = 0x0184
	lbSetCurSel    = 0x0186
	lbGetCurSel    = 0x0188
	lbSetItemHt    = 0x01A0

	odsSelected = 0x0001
	odsDisabled = 0x0004
	odtListBox  = 2
	odtButton   = 4

	dtLeft      = 0x0000
	dtRight     = 0x0002
	dtCenter    = 0x0001
	dtVCenter   = 0x0004
	dtSingleLn  = 0x0020
	dtNoPrefix  = 0x0800
	dtEndEllips = 0x8000

	bnClicked = 0
	lbnSelChg = 1

	swShow    = 5
	idcArrow  = 32512
	idiApp    = 32512
	richClass = "RICHEDIT50W"
	richDll   = "Msftedit.dll"

	ofnHideReadOnly     = 0x00000004
	ofnNoChangeDir      = 0x00000008
	ofnPathMustExist    = 0x00000800
	ofnFileMustExist    = 0x00001000
	ofnAllowMultiSelect = 0x00000200
	ofnExplorer         = 0x00080000
)

// 莫兰迪配色（COLORREF = 0x00BBGGRR），跟 keymap 那边保持一致
const (
	clrBg      = 0x00E2E8ED
	clrCard    = 0x00FFFFFF
	clrBorder  = 0x00C9D3DB
	clrHeader  = 0x00D4DDE4
	clrBtnBg   = 0x00DAE1E7
	clrBtnBgP  = 0x00C9D1D8
	clrPrimary = 0x00A69A7E
	clrPrimaP  = 0x0092876B
	clrSelBg   = 0x00E7DFD3
	clrText    = 0x00454A4F
	clrMuted   = 0x007F858B
	clrLabel   = 0x003F464D
	clrWhite   = 0x00FFFFFF
	clrOK      = 0x00809A7D
	clrBad     = 0x00727BAE
	clrGray    = 0x008D949A
	clrType    = 0x008B7F6F
	clrHit     = 0x008B8B6E
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	comdlg32 = syscall.NewLazyDLL("comdlg32.dll")

	procRegisterClassExW   = user32.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32.NewProc("CreateWindowExW")
	procDefWindowProcW     = user32.NewProc("DefWindowProcW")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procShowWindow         = user32.NewProc("ShowWindow")
	procUpdateWindow       = user32.NewProc("UpdateWindow")
	procPeekMessageW       = user32.NewProc("PeekMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procPostQuitMessage    = user32.NewProc("PostQuitMessage")
	procPostMessageW       = user32.NewProc("PostMessageW")
	procSendMessageW       = user32.NewProc("SendMessageW")
	procSetWindowTextW     = user32.NewProc("SetWindowTextW")
	procGetClientRect      = user32.NewProc("GetClientRect")
	procMoveWindow         = user32.NewProc("MoveWindow")
	procLoadCursorW        = user32.NewProc("LoadCursorW")
	procLoadIconW          = user32.NewProc("LoadIconW")
	procMessageBoxW        = user32.NewProc("MessageBoxW")
	procSetFocus           = user32.NewProc("SetFocus")
	procGetDC              = user32.NewProc("GetDC")
	procReleaseDC          = user32.NewProc("ReleaseDC")
	procInvalidateRect     = user32.NewProc("InvalidateRect")
	procRedrawWindow       = user32.NewProc("RedrawWindow")
	procBeginPaint         = user32.NewProc("BeginPaint")
	procEndPaint           = user32.NewProc("EndPaint")
	procFillRect           = user32.NewProc("FillRect")
	procDrawTextW          = user32.NewProc("DrawTextW")
	procSetWindowRgn       = user32.NewProc("SetWindowRgn")
	procSetWindowLongPtrW  = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProcW    = user32.NewProc("CallWindowProcW")
	procSetProcessDPIAware = user32.NewProc("SetProcessDPIAware")
	procOpenClipboard      = user32.NewProc("OpenClipboard")
	procEmptyClipboard     = user32.NewProc("EmptyClipboard")
	procSetClipboardData   = user32.NewProc("SetClipboardData")
	procCloseClipboard     = user32.NewProc("CloseClipboard")

	procCreateFontW        = gdi32.NewProc("CreateFontW")
	procCreateSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	procCreatePen          = gdi32.NewProc("CreatePen")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procRoundRect          = gdi32.NewProc("RoundRect")
	procSetTextColor       = gdi32.NewProc("SetTextColor")
	procSetBkColor         = gdi32.NewProc("SetBkColor")
	procSetBkMode          = gdi32.NewProc("SetBkMode")
	procCreateRoundRgn     = gdi32.NewProc("CreateRoundRectRgn")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatBmp    = gdi32.NewProc("CreateCompatibleBitmap")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procBitBlt             = gdi32.NewProc("BitBlt")
	procGetDeviceCaps      = gdi32.NewProc("GetDeviceCaps")
	procGetStockObject     = gdi32.NewProc("GetStockObject")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procLoadLibraryW     = kernel32.NewProc("LoadLibraryW")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procRtlMoveMemory    = kernel32.NewProc("RtlMoveMemory")

	procDragAcceptFiles = shell32.NewProc("DragAcceptFiles")
	procDragQueryFileW  = shell32.NewProc("DragQueryFileW")
	procDragFinish      = shell32.NewProc("DragFinish")

	procGetOpenFileNameW = comdlg32.NewProc("GetOpenFileNameW")
)

type wndClassEx struct {
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

type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

type rectT struct{ left, top, right, bottom int32 }

type minMaxInfo struct {
	reserved     [2]int32
	maxSize      [2]int32
	maxPosition  [2]int32
	minTrackSize [2]int32
	maxTrackSize [2]int32
}

type drawItem struct {
	CtlType    uint32
	CtlID      uint32
	itemID     uint32
	itemAction uint32
	itemState  uint32
	hwndItem   uintptr
	hDC        uintptr
	rcItem     rectT
	itemData   uintptr
}

type measureItem struct {
	CtlType    uint32
	CtlID      uint32
	itemID     uint32
	itemWidth  uint32
	itemHeight uint32
	itemData   uintptr
}

type charFormat2 struct {
	cbSize         uint32
	dwMask         uint32
	dwEffects      uint32
	yHeight        int32
	yOffset        int32
	crTextColor    uint32
	bCharSet       byte
	bPitchAndFam   byte
	szFaceName     [32]uint16
	wWeight        uint16
	sSpacing       int16
	crBackColor    uint32
	lcid           uint32
	dwReserved     uint32
	sStyle         int16
	wKerning       uint16
	bUnderlineType byte
	bAnimation     byte
	bRevAuthor     byte
	bReserved      byte
}

type setTextEx struct {
	flags    uint32
	codepage uint32
}

type openFileNameW struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	flagsEx           uint32
}

// ---------------------------------------------------------------- 数据

// guiItem 是列表里的一行：某个文件的某张表的某条记录。
// 文件解析失败时 tab 为 nil，靠 f.err 显示原因。
type guiItem struct {
	f   *guiFile
	tab *Table
	rec Rec
	hdr bool // 多文件时的“文件分组”行，不可当颗粒看
}

type guiFile struct {
	path string
	fr   *FileResult
	err  string
}

type guiT struct {
	hwnd   uintptr
	hInfo  uintptr
	hList  uintptr
	hDet   uintptr
	hOpen  uintptr
	hClear uintptr
	hCopy  uintptr
	hChk   uintptr

	fontUI     uintptr
	fontPaint  uintptr
	fontList   uintptr
	fontMono   uintptr
	fontSmall  uintptr
	richLoaded bool

	mu      sync.Mutex
	busy    bool
	done    bool
	pending []*guiFile
	perr    string

	selfTest   string
	dropTarget string

	files     []*guiFile
	items     []guiItem
	sel       int
	sortSpeed bool
	status    string

	headerRect rectT
	infoRect   rectT
	listRect   rectT
	detRect    rectT
	statusRect rectT
}

var (
	currentGUI *guiT
	dpiScale   = 1.0
)

func S(v int32) int32 { return int32(float64(v)*dpiScale + 0.5) }

func initDPI() {
	procSetProcessDPIAware.Call()
	hdc, _, _ := procGetDC.Call(0)
	if hdc != 0 {
		v, _, _ := procGetDeviceCaps.Call(hdc, 88) // LOGPIXELSY
		procReleaseDC.Call(0, hdc)
		if v >= 96 && v <= 480 {
			dpiScale = float64(v) / 96.0
		}
	}
}

// ---------------------------------------------------------------- 小工具

func u16ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		empty := []uint16{0}
		return &empty[0]
	}
	return p
}

// utf16FromStr 手工转 UTF-16：syscall.StringToUTF16 遇到 NUL 会 panic，
// 而文件对话框的 filter 本身就用 NUL 分隔。
func utf16FromStr(s string) []uint16 {
	u := utf16.Encode([]rune(s))
	return append(u, 0)
}

func toCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// wrapPath 把过长的路径按反斜杠折行，续行缩进对齐值列（详情面板是等宽字体，按字符算宽）
func wrapPath(s, indent string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	var lines []string
	for len(r) > width {
		cut := width
		for i := width; i > 0; i-- {
			if r[i-1] == '\\' || r[i-1] == '/' {
				cut = i
				break
			}
		}
		lines = append(lines, string(r[:cut]))
		r = r[cut:]
	}
	lines = append(lines, string(r))
	return strings.Join(lines, "\n"+indent)
}

// runeCols：一个汉字（全角）算 2 列，ASCII 算 1 列
func runeCols(r rune) int {
	switch {
	case r < 0x1100:
		return 1
	case r <= 0x115F,
		r >= 0x2E80 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

func textCols(s string) int {
	n := 0
	for _, c := range s {
		n += runeCols(c)
	}
	return n
}

// wrapCols 给中文段落折行：按显示宽度（汉字 2 列）算，优先在标点/空格后断开。
// wrapPath 是按字符数折路径的，中文用它会把行拉得太长。
func wrapCols(s, indent string, cols int) string {
	if cols < 12 {
		cols = 12
	}
	if textCols(s) <= cols {
		return s
	}
	r := []rune(s)
	var lines []string
	for {
		col, cut := 0, len(r)
		for i, c := range r {
			col += runeCols(c)
			if col > cols {
				cut = i
				break
			}
		}
		if cut == len(r) {
			lines = append(lines, string(r))
			break
		}
		if cut < 1 {
			cut = 1
		}
		best := cut
		for i := cut; i > cut/2 && i > 0; i-- {
			switch r[i-1] {
			case ' ', '/', '\\', '，', '；', '、', '）', '：', '。':
				best = i
				i = 0
			}
		}
		if best < 1 {
			best = 1
		}
		lines = append(lines, strings.TrimRight(string(r[:best]), " "))
		r = r[best:]
		if len(r) == 0 {
			break
		}
	}
	return strings.Join(lines, "\n"+indent)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// guiTrace 只在设置了 MTKEMI_GUI_LOG 环境变量时写日志
func guiTrace(format string, a ...any) {
	path := os.Getenv("MTKEMI_GUI_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] ", time.Now().Format("15:04:05.000"))
	fmt.Fprintf(f, format, a...)
	fmt.Fprintln(f)
}

func writeCrashLog(s string) {
	p := filepath.Join(os.TempDir(), "mtkemi_crash.log")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s]\n%s\n", time.Now().Format("2006-01-02 15:04:05"), s)
}

func mkFont(name string, height, weight int32) uintptr {
	h, _, _ := procCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0,
		134, 0, 0, 5, 0, uintptr(unsafe.Pointer(u16ptr(name))))
	return h
}

func fillRect(hdc uintptr, r rectT, color uint32) {
	br, _, _ := procCreateSolidBrush.Call(uintptr(color))
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&r)), br)
	procDeleteObject.Call(br)
}

func fillRound(hdc uintptr, r rectT, radius int32, color uint32) {
	br, _, _ := procCreateSolidBrush.Call(uintptr(color))
	pen, _, _ := procCreatePen.Call(0, 1, uintptr(color))
	ob, _, _ := procSelectObject.Call(hdc, br)
	op, _, _ := procSelectObject.Call(hdc, pen)
	procRoundRect.Call(hdc, uintptr(r.left), uintptr(r.top), uintptr(r.right), uintptr(r.bottom),
		uintptr(radius*2), uintptr(radius*2))
	procSelectObject.Call(hdc, ob)
	procSelectObject.Call(hdc, op)
	procDeleteObject.Call(br)
	procDeleteObject.Call(pen)
}

func strokeRound(hdc uintptr, r rectT, radius int32, color uint32) {
	null, _, _ := procGetStockObject.Call(5) // NULL_BRUSH
	pen, _, _ := procCreatePen.Call(0, 1, uintptr(color))
	ob, _, _ := procSelectObject.Call(hdc, null)
	op, _, _ := procSelectObject.Call(hdc, pen)
	procRoundRect.Call(hdc, uintptr(r.left), uintptr(r.top), uintptr(r.right), uintptr(r.bottom),
		uintptr(radius*2), uintptr(radius*2))
	procSelectObject.Call(hdc, ob)
	procSelectObject.Call(hdc, op)
	procDeleteObject.Call(pen)
}

func drawLabel(hdc uintptr, r rectT, s string, color uint32, flags uint32) {
	procSetBkMode.Call(hdc, 1) // TRANSPARENT
	procSetTextColor.Call(hdc, uintptr(color))
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16ptr(s))), ^uintptr(0),
		uintptr(unsafe.Pointer(&r)), uintptr(flags))
}

// ---------------------------------------------------------------- 富文本

type span struct {
	start, end int
	color      uint32
	bold       bool
}

func cpMap(s string) []int {
	m := make([]int, len(s)+1)
	i, cp := 0, 0
	for i < len(s) {
		m[i] = cp
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\r':
			cp += 2
		case r >= 0x10000:
			cp += 2
		default:
			cp++
		}
		for k := 1; k < w; k++ {
			m[i+k] = cp
		}
		i += w
	}
	m[len(s)] = cp
	return m
}

// labelSpans 把每行 “标签 : 值” 里的标签部分加粗上色
func labelSpans(s string, lineColor uint32) []span {
	var out []span
	base := 0
	for base <= len(s) {
		end := strings.IndexByte(s[base:], '\n')
		line := s[base:]
		if end >= 0 {
			line = s[base : base+end]
		}
		le := len(line)
		for le > 0 && (line[le-1] == '\r' || line[le-1] == ' ') {
			le--
		}
		line = line[:le]

		// 标题行（没有 “  :  ” 且是首行）用主色加粗
		if k := strings.Index(line, " : "); k > 0 && k < 34 {
			out = append(out, span{base, base + k + 1, clrLabel, true})
		} else if base == 0 && line != "" {
			out = append(out, span{base, base + len(line), lineColor, true})
		} else if strings.HasPrefix(line, "  ") && strings.Contains(line, "0x") {
			// 寄存器行：名字部分上色
			if k := strings.Index(line, "0x"); k > 2 {
				out = append(out, span{base + 2, base + k - 1, clrType, false})
			}
		}
		if end < 0 {
			break
		}
		base += end + 1
	}
	return out
}

func richPut(h uintptr, s, face string, pt int, spans []span) {
	if h == 0 {
		return
	}
	procSendMessageW.Call(h, wmSetRedraw, 0, 0)
	procSendMessageW.Call(h, emExLimitText, 0, 0x7FFFFFFF)

	var cf charFormat2
	cf.cbSize = uint32(unsafe.Sizeof(cf))
	cf.dwMask = cfmFace | cfmSize
	cf.yHeight = 20 * int32(pt)
	copy(cf.szFaceName[:], utf16.Encode([]rune(face)))
	procSendMessageW.Call(h, emSetSel, 0, ^uintptr(0))
	procSendMessageW.Call(h, emSetCharFormat, scfAll, uintptr(unsafe.Pointer(&cf)))

	st := setTextEx{flags: 0, codepage: 1200}
	procSendMessageW.Call(h, emSetTextEx, uintptr(unsafe.Pointer(&st)),
		uintptr(unsafe.Pointer(u16ptr(toCRLF(s)))))
	procSendMessageW.Call(h, emSetSel, 0, 0)

	cm := cpMap(toCRLF(s))
	_ = cm
	cm2 := cpMap(s)
	for _, sp := range spans {
		if sp.start >= sp.end || sp.end > len(cm2)-1 {
			continue
		}
		var f charFormat2
		f.cbSize = uint32(unsafe.Sizeof(f))
		f.dwMask = cfmColor
		if sp.bold {
			f.dwMask |= cfmBold | cfmWeight
			f.dwEffects = cfeBold
			f.wWeight = 700
		}
		f.crTextColor = sp.color
		procSendMessageW.Call(h, emSetSel, uintptr(cm2[sp.start]), uintptr(cm2[sp.end]))
		procSendMessageW.Call(h, emSetCharFormat, scfSelection, uintptr(unsafe.Pointer(&f)))
	}
	procSendMessageW.Call(h, emSetSel, 0, 0)
	procSendMessageW.Call(h, emScrollCaret, 0, 0)
	procSendMessageW.Call(h, wmSetRedraw, 1, 0)
	procInvalidateRect.Call(h, 0, 1)
}

// ---------------------------------------------------------------- 业务：列表

func (g *guiT) curItem() (guiItem, bool) {
	if g.sel >= 0 && g.sel < len(g.items) {
		return g.items[g.sel], true
	}
	return guiItem{}, false
}

func (g *guiT) buildItems() {
	g.items = g.items[:0]
	group := len(g.files) > 1 && !g.sortSpeed
	for _, f := range g.files {
		if f.err != "" || f.fr == nil || len(f.fr.Tables) == 0 {
			g.items = append(g.items, guiItem{f: f})
			continue
		}
		if group {
			g.items = append(g.items, guiItem{f: f, hdr: true})
		}
		for _, t := range f.fr.Tables {
			for _, r := range t.Records {
				g.items = append(g.items, guiItem{f: f, tab: t, rec: r})
			}
		}
	}

	if g.sortSpeed {
		sort.SliceStable(g.items, func(i, j int) bool {
			a, b := g.items[i], g.items[j]
			if (a.tab == nil) != (b.tab == nil) {
				return a.tab != nil
			}
			if a.tab == nil {
				return false
			}
			if a.rec.Valid != b.rec.Valid {
				return a.rec.Valid
			}
			ra, rb := rateOf(a.rec.Type), rateOf(b.rec.Type)
			if ra != rb {
				return ra > rb
			}
			ca, cb := totalCap(a.rec.Ranks), totalCap(b.rec.Ranks)
			if ca != cb {
				return ca > cb
			}
			return false
		})
	}
}

// listRow 是列表每一项的两行文字，也给 -guitest 导出用
func listRow(it guiItem) (string, string) {
	if it.hdr {
		n, head := 0, ""
		if it.f.fr != nil {
			for i, t := range it.f.fr.Tables {
				n += len(t.Records)
				if i == 0 {
					head = t.RawHead
				}
			}
		}
		return filepath.Base(it.f.path), fmt.Sprintf("文件 · %d 条记录 · %s", n, head)
	}
	if it.tab == nil {
		return filepath.Base(it.f.path) + "  ⚠ 无法解析", oneLine(it.f.err)
	}
	line1 := fmt.Sprintf("#%d  %s", it.rec.Idx, pnmText(it.rec))
	if !it.rec.Valid && it.rec.Note != "" {
		line1 += "  ⚠"
	}
	line2 := fmt.Sprintf("%s · %s · %s · %s · %s",
		vendorOf(it.rec.ID, it.rec.IDLen),
		packKind(it.rec.Type),
		mediaKind(it.rec, it.tab.Suffix),
		ddrKind(it.rec.Type),
		composition(it.rec.Ranks))
	return line1, line2
}

// ---------------------------------------------------------------- 业务：信息卡

func (g *guiT) infoText() string {
	it, ok := g.curItem()
	if !ok {
		return "还没有打开文件\n\n" +
			"· 把 preloader_xxx.bin / boot1.bin / PRELOADER.img 拖进窗口\n" +
			"· 也可以直接拖整个设备备份目录，会自动找 preloader*\n" +
			"· 或者点左上角「选择文件…」\n\n" +
			"这里会显示当前文件的目录、大小和它支持的闪存汇总。\n\n" +
			"数据库 : " + DBStatus()
	}
	f := it.f
	name := filepath.Base(f.path)
	dir := filepath.Dir(f.path)

	var b strings.Builder
	kv := func(label, val string) { fmt.Fprintf(&b, "%s : %s\n", pad(label, 9), val) }

	fmt.Fprintf(&b, "%s\n", name)
	kv("目录", wrapPath(dir, strings.Repeat(" ", 12), 44))

	if f.err != "" || f.fr == nil {
		if f.fr != nil && (f.fr.Platform.Name != "" || f.fr.Platform.Basis != "") {
			kv("平台", wrapCols(platDisplay(f.fr.Platform), strings.Repeat(" ", 12), 50))
		}
		fmt.Fprintf(&b, "\n无法解析 : %s\n", oneLine(f.err))
		return b.String()
	}

	// 文件里头写着的 preloader 名字：文件名被改成 boot1.bin / PRELOADER.img 也能认出来是哪份
	t0 := it.tab
	if t0 == nil && len(f.fr.Tables) > 0 {
		t0 = f.fr.Tables[0]
	}
	pn := ""
	if t0 != nil {
		pn = strings.TrimSpace(t0.PreName)
	}
	if pn == "" {
		pn = "（表里没写）"
	}
	kv("preloader", wrapPath(pn, strings.Repeat(" ", 12), 42))

	// 平台是靠文件名/文件内字符串猜的，界面上明确写出“不一定准”
	kv("平台", wrapCols(platDisplay(f.fr.Platform), strings.Repeat(" ", 12), 50))

	kv("大小", fmt.Sprintf("%s (%d 字节)", f.fr.SizeHex, f.fr.Size))
	if len(g.files) > 1 {
		for i, x := range g.files {
			if x == f {
				kv("文件", fmt.Sprintf("第 %d / %d 个", i+1, len(g.files)))
			}
		}
	}
	if len(f.fr.Tables) == 0 {
		fmt.Fprintf(&b, "\n文件里没有 MTK_BLOADER_INFO 表\n")
		return b.String()
	}
	if t0 != nil {
		if t0.Truncated && t0.CountWant > 0 {
			kv("表", fmt.Sprintf("%s   %d/%d 条 × %d 字节", t0.RawHead, t0.Count, t0.CountWant, t0.RecSize))
		} else {
			kv("表", fmt.Sprintf("%s   %d 条 × %d 字节", t0.RawHead, t0.Count, t0.RecSize))
		}
		if t0.Note != "" {
			kv("提示", wrapCols("⚠ "+t0.Note, strings.Repeat(" ", 12), 50))
		}
	}

	total, valid := 0, 0
	ddr, media, pack, vendor := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	var minC, maxC uint64
	first := true
	for _, t := range f.fr.Tables {
		for _, r := range t.Records {
			total++
			if !r.Valid {
				continue
			}
			valid++
			ddr[ddrKind(r.Type)]++
			media[mediaKind(r, t.Suffix)]++
			pack[packKind(r.Type)]++
			vendor[vendorOf(r.ID, r.IDLen)]++
			if c := totalCap(r.Ranks); c > 0 {
				if first || c < minC {
					minC = c
				}
				if first || c > maxC {
					maxC = c
				}
				first = false
			}
		}
	}
	kv("有效", fmt.Sprintf("%d / %d 条", valid, total))
	if valid > 0 {
		kv("DDR", countLine(ddr))
		kv("介质", countLine(media))
		kv("封装", countLine(pack))
		kv("厂商", countLine(vendor))
		if !first {
			if minC == maxC {
				kv("容量", humanSize(maxC))
			} else {
				kv("容量", humanSize(minC)+" ~ "+humanSize(maxC))
			}
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- 业务：详情

// actimeClockHint：v30+ 的 ACTIME0 低 16 位在 20 条样本里有 17 条正好等于该代际
// 标称时钟（LPDDR3 933 / 800），但存在反例，所以这里只回显数值，不冒充实测频率。
func actimeClockHint(ver int, names []string, w []uint32) string {
	if ver < 30 {
		return ""
	}
	for i, n := range names {
		if n == "DRAMC_ACTIME0" && i < len(w) && w[i] != 0 {
			lo := w[i] & 0xFFFF
			return fmt.Sprintf("（ACTIME0 低 16 位 = 0x%04X = %d，可能对应 DRAM 时钟 MHz，未证实）", lo, lo)
		}
	}
	return ""
}

func (g *guiT) detailText() string {
	it, ok := g.curItem()
	if !ok {
		return "左侧还没有条目。\n\n" +
			"把 preloader_xxx.bin / boot1.bin / PRELOADER.img 拖进窗口，\n" +
			"或者点上面的「选择文件…」。\n\n" +
			"拖整个设备目录也行，会自动在里面找 preloader*（最多 40 个）。"
	}
	if it.hdr {
		return "这个文件里一共 " + strconv.Itoa(func() int {
			n := 0
			if it.f.fr != nil {
				for _, t := range it.f.fr.Tables {
					n += len(t.Records)
				}
			}
			return n
		}()) + " 条记录。\n\n点左边的具体条目看每颗闪存的型号 / DDR 版本 / 容量 / 时序寄存器。\n\n" + g.infoText()
	}
	if it.tab == nil {
		return "这个文件解析不了：\n\n" + it.f.err + "\n\n文件 : " + it.f.path
	}
	r, t := it.rec, it.tab

	var b strings.Builder
	title := pnmOf(r)
	if title == "" {
		title = idCell(r)
	}
	fmt.Fprintf(&b, "%s\n", title)
	fmt.Fprintf(&b, "%s · %s · %s\n\n", vendorOf(r.ID, r.IDLen), packKind(r.Type), mediaKind(r, t.Suffix))

	kvv := func(label, val string) {
		fmt.Fprintf(&b, "%s : %s\n", pad(label, 15), val)
	}
	ind := strings.Repeat(" ", 18)
	kvvIf := func(label, val string) {
		if strings.TrimSpace(val) == "" {
			return
		}
		kvv(label, wrapCols(val, ind, 72))
	}

	// 型号库那几行按标签取出来，好和第几行按「类型 → 厂商 → PNM → 丝印 → eMMC → CID → ID → 速度 → 推荐 → 年份设备 → 备注」排
	dbv := map[string]string{}
	var dbRest [][2]string
	for _, l := range DBDetailLines(pnmOf(r), r.ID, int(r.IDLen), totalCap(r.Ranks)) {
		switch l[0] {
		case "丝印型号", "eMMC版本", "实测速度", "推荐指数", "常见年份与设备", "备注", "匹配依据":
			dbv[l[0]] = l[1]
		default:
			dbRest = append(dbRest, l)
		}
	}

	kvv("类型", fmt.Sprintf("0x%03X   %s / %s / %s",
		r.Type, ddrKind(r.Type), packKind(r.Type), mediaKind(r, t.Suffix)))
	kvv("厂商", vendorOf(r.ID, r.IDLen))
	kvv("PNM", pnmText(r))
	kvvIf("丝印型号", dbv["丝印型号"])
	kvvIf("eMMC版本", dbv["eMMC版本"])
	if c := cidDecode(r.ID, r.IDLen); c != "" {
		kvv("CID", c)
	}
	kvv("ID", fmt.Sprintf("%d 字节   %s", r.IDLen, hexID(r.ID, r.IDLen)))
	kvvIf("实测速度", dbv["实测速度"])
	kvvIf("推荐指数", dbv["推荐指数"])
	kvvIf("常见年份与设备", dbv["常见年份与设备"])
	kvvIf("备注", dbv["备注"])
	if p := partNo(r.ID, r.IDLen); p != "" {
		kvv("料号", p)
	}
	kvv("DRAM", fmt.Sprintf("%s   代际标称 %s", ddrKind(r.Type), rateText(r.Type)))
	kvv("容量", humanSize(totalCap(r.Ranks)))
	kvv("组成", composition(r.Ranks))
	kvv("rank", "["+joinHex(r.Ranks)+"]")
	if !r.Valid {
		kvv("注意", "⚠ "+r.Note)
	}
	for _, l := range dbRest {
		kvv(l[0], wrapCols(l[1], ind, 72))
	}
	kvvIf("匹配依据", dbv["匹配依据"])

	b.WriteString("\n时序寄存器\n")
	names := wordNames(t.Version, len(r.Words))
	zeros := 0
	for i, w := range r.Words {
		if w == 0 {
			zeros++
			continue
		}
		n := fmt.Sprintf("W%d", i)
		if i < len(names) {
			n = names[i]
		}
		fmt.Fprintf(&b, "  %s 0x%08X\n", pad(n, 17), w)
	}
	// v30+ 记录里 ACTIME0 非零时，低 16 位看起来像 DRAM 时钟 MHz（933/800…），
	// 但样本里还有例外，没证实，所以只把原始值摆出来，不当结论
	if n := actimeClockHint(t.Version, names, r.Words); n != "" {
		b.WriteString("  " + n + "\n")
	}

	switch {
	case len(r.Words) == 0:
		b.WriteString("  （这张表没有寄存器字段）\n")
	case zeros == len(r.Words):
		b.WriteString("  （全部为 0：这份 preloader 没写时序寄存器）\n")
	case zeros > 0:
		fmt.Fprintf(&b, "  （其余 %d 个为 0）\n", zeros)
	}

	if len(r.MR) > 0 {
		b.WriteString("\nLPDDR MR\n  ")
		b.WriteString(mrText(r.MR, true))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if pn := strings.TrimSpace(t.PreName); pn != "" {
		kvv("preloader", pn)
	}
	if pf := it.f.fr.Platform; pf.Name != "" || pf.Basis != "" {
		kvv("平台", wrapCols(platDisplay(pf), ind, 72))
	}
	if t.Note != "" {
		kvv("提示", wrapCols("⚠ "+t.Note, ind, 72))
	}
	kvv("所在表", fmt.Sprintf("%s @ 0x%X   记录 #%d", t.RawHead, t.Offset, r.Idx))
	kvv("文件", wrapPath(it.f.path, ind, 80))
	if r.Valid {
		_ = r
	}
	return b.String()
}

// ---------------------------------------------------------------- 刷新

func (g *guiT) setStatus(s string) {
	g.status = s
	procInvalidateRect.Call(g.hwnd, 0, 1)
}

func (g *guiT) refreshInfo() {
	txt := g.infoText()
	richPut(g.hInfo, txt, "Microsoft YaHei UI", 10, labelSpans(txt, clrPrimary))
	// 行的行数变了，信息卡高度要跟着变
	if g.hwnd != 0 && g.hList != 0 {
		var rc rectT
		procGetClientRect.Call(g.hwnd, uintptr(unsafe.Pointer(&rc)))
		if rc.right > 0 && rc.bottom > 0 {
			g.layout(rc.right, rc.bottom)
		}
	}
}

func (g *guiT) refreshDetail() {
	txt := g.detailText()
	richPut(g.hDet, txt, "新宋体", 10, labelSpans(txt, clrPrimary))
}

// refreshList 重建列表控件并保持选中项
func (g *guiT) refreshList() {
	keep := -1
	if g.sel >= 0 && g.sel < len(g.items) {
		keep = g.sel
	}
	g.buildItems()
	procSendMessageW.Call(g.hList, lbResetContent, 0, 0)
	for _, it := range g.items {
		l1, l2 := listRow(it)
		procSendMessageW.Call(g.hList, lbAddString, 0, uintptr(unsafe.Pointer(u16ptr(l1+"  "+l2))))
	}
	if len(g.items) == 0 {
		g.sel = -1
	} else if keep >= 0 && keep < len(g.items) {
		g.sel = keep
	} else {
		g.sel = 0
	}
	if g.sel >= 0 {
		procSendMessageW.Call(g.hList, lbSetCurSel, uintptr(g.sel), 0)
	}
	g.refreshInfo()
	g.refreshDetail()

	n := 0
	for _, f := range g.files {
		if f.err == "" && f.fr != nil {
			n++
		}
	}
	switch {
	case len(g.items) == 0:
		g.setStatus("把 preloader / boot1 拖进来，或点「选择文件…」")
	default:
		hint := ""
		if g.sortSpeed {
			hint = "｜已按代际标称速率排序（不是实测频率）"
		}
		g.setStatus(fmt.Sprintf("%d 个文件 · %d 条颗粒｜点左侧条目看详情%s", n, len(g.items), hint))
	}
}

// loadFiles 解析（后台线程），完成后回 UI 线程刷新
func (g *guiT) loadFiles(input []string) {
	paths := expandPaths(input)
	g.mu.Lock()
	if g.busy {
		g.mu.Unlock()
		return
	}
	g.busy = true
	g.mu.Unlock()

	guiTrace("loadFiles %v", paths)
	g.setStatus("正在解析 …")

	go func() {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 8192)
				n := runtime.Stack(buf, false)
				g.mu.Lock()
				g.perr = fmt.Sprintf("内部错误: %v\n%s", r, buf[:n])
				g.mu.Unlock()
				procPostMessageW.Call(g.hwnd, wmAppDone, 0, 0)
			}
		}()

		opts := Options{Sort: "idx", Verbose: true, QuietReport: true}
		var files []*guiFile
		for _, p := range paths {
			guiTrace("parse %s", p)
			fr := processFile(p, opts)
			files = append(files, &guiFile{path: p, fr: fr, err: fr.Error})
		}

		g.mu.Lock()
		g.pending = files
		g.perr = ""
		g.mu.Unlock()
		procPostMessageW.Call(g.hwnd, wmAppDone, 0, 0)
	}()
}

func (g *guiT) finishAsync() {
	g.mu.Lock()
	files := g.pending
	g.pending = nil
	perr := g.perr
	g.busy = false
	g.done = true
	g.mu.Unlock()

	if perr != "" {
		g.files = nil
		g.items = nil
		g.sel = -1
		richPut(g.hDet, "内部错误：\n\n"+perr, "新宋体", 10, nil)
		return
	}
	g.files = files
	g.sel = 0
	g.refreshList()
	guiTrace("finishAsync: %d files, %d items", len(g.files), len(g.items))
}

// ---------------------------------------------------------------- 拖放

func expandPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			out = append(out, p)
			continue
		}
		before := len(out)
		_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if len(out) >= 40 {
				return filepath.SkipAll
			}
			name := strings.ToLower(d.Name())
			if strings.HasPrefix(name, "preloader") || name == "boot1.bin" {
				out = append(out, path)
			}
			return nil
		})
		if len(out) == before {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- 绘制

func (g *guiT) paint(hwnd uintptr) {
	var ps [128]byte
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps[0])))
	if hdc == 0 {
		return
	}
	var cr rectT
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))

	memDC, _, _ := procCreateCompatibleDC.Call(hdc)
	bmp, _, _ := procCreateCompatBmp.Call(hdc, uintptr(cr.right), uintptr(cr.bottom))
	oldBmp, _, _ := procSelectObject.Call(memDC, bmp)
	fillRect(memDC, rectT{0, 0, cr.right, cr.bottom}, clrBg)

	fillRound(memDC, g.headerRect, S(10), clrHeader)
	fillRound(memDC, g.infoRect, S(10), clrCard)
	fillRound(memDC, g.listRect, S(10), clrCard)
	fillRound(memDC, g.detRect, S(10), clrCard)
	strokeRound(memDC, g.infoRect, S(10), clrBorder)
	strokeRound(memDC, g.listRect, S(10), clrBorder)
	strokeRound(memDC, g.detRect, S(10), clrBorder)

	old, _, _ := procSelectObject.Call(memDC, g.fontPaint)
	hr := g.headerRect
	hr.left += S(18)
	hr.right -= S(18)
	drawLabel(memDC, hr, "MTK EMI 解析器", clrText, dtLeft|dtVCenter|dtSingleLn)
	hr.left += S(168)
	drawLabel(memDC, hr, "preloader / boot1 → 支持的闪存型号 · DDR 版本 · 容量 · 时序", clrMuted, dtLeft|dtVCenter|dtSingleLn)
	procSelectObject.Call(memDC, old)

	// 卡片小标题
	lab := func(r rectT, s string) {
		lr := rectT{r.left + S(14), r.top + S(8), r.right - S(14), r.top + S(26)}
		o, _, _ := procSelectObject.Call(memDC, g.fontSmall)
		drawLabel(memDC, lr, s, clrMuted, dtLeft|dtVCenter|dtSingleLn)
		procSelectObject.Call(memDC, o)
	}
	lab(g.detRect, "详情")

	o2, _, _ := procSelectObject.Call(memDC, g.fontSmall)
	drawLabel(memDC, g.statusRect, g.status, clrMuted, dtLeft|dtVCenter|dtSingleLn)
	procSelectObject.Call(memDC, o2)

	procBitBlt.Call(hdc, 0, 0, uintptr(cr.right), uintptr(cr.bottom), memDC, 0, 0, 0x00CC0020)
	procSelectObject.Call(memDC, oldBmp)
	procDeleteObject.Call(bmp)
	procDeleteDC.Call(memDC)
	procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps[0])))
}

func (g *guiT) paintButton(di *drawItem) {
	r := di.rcItem
	pressed := di.itemState&odsSelected != 0
	var bg, fg uint32 = clrBtnBg, clrText
	switch di.CtlID {
	case idBtnOpen:
		bg, fg = clrPrimary, clrWhite
		if pressed {
			bg = clrPrimaP
		}
	default:
		if pressed {
			bg = clrBtnBgP
		}
	}
	fillRound(di.hDC, r, S(8), bg)
	old, _, _ := procSelectObject.Call(di.hDC, g.fontUI)
	drawLabel(di.hDC, r, btnText[di.CtlID], fg, dtCenter|dtVCenter|dtSingleLn)
	procSelectObject.Call(di.hDC, old)
}

var btnText = map[uint32]string{
	idBtnOpen:  "选择文件…",
	idBtnClear: "清空",
	idBtnCopy:  "复制详情",
}

func (g *guiT) paintListItem(di *drawItem) {
	idx := int(di.itemID)
	if idx < 0 || idx >= len(g.items) {
		return
	}
	it := g.items[idx]
	r := di.rcItem
	sel := di.itemState&odsSelected != 0

	bg, fg, sub := uint32(clrCard), uint32(clrText), uint32(clrMuted)
	if sel {
		bg, fg, sub = clrSelBg, clrPrimary, clrPrimary
	}
	switch {
	case it.hdr:
		fg, sub = clrText, clrMuted
	case it.tab == nil:
		fg, sub = clrBad, clrMuted
	}
	fillRect(di.hDC, r, bg)

	rightW := S(92)
	l1 := rectT{r.left + S(12), r.top + S(3), r.right - rightW, r.top + S(25)}
	l2 := rectT{r.left + S(12), r.top + S(23), r.right - rightW, r.top + S(45)}

	a, b := listRow(it)
	old, _, _ := procSelectObject.Call(di.hDC, g.fontList)
	drawLabel(di.hDC, l1, a, fg, dtLeft|dtVCenter|dtSingleLn|dtEndEllips|dtNoPrefix)
	procSelectObject.Call(di.hDC, old)

	old, _, _ = procSelectObject.Call(di.hDC, g.fontSmall)
	drawLabel(di.hDC, l2, b, sub, dtLeft|dtVCenter|dtSingleLn|dtEndEllips|dtNoPrefix)
	procSelectObject.Call(di.hDC, old)

	if it.tab != nil {
		cr := rectT{r.right - rightW, r.top + S(3), r.right - S(12), r.top + S(25)}
		sr := rectT{r.right - rightW, r.top + S(23), r.right - S(12), r.top + S(45)}
		old, _, _ = procSelectObject.Call(di.hDC, g.fontList)
		drawLabel(di.hDC, cr, humanSize(totalCap(it.rec.Ranks)), fg, dtRight|dtVCenter|dtSingleLn)
		procSelectObject.Call(di.hDC, old)
		old, _, _ = procSelectObject.Call(di.hDC, g.fontSmall)
		drawLabel(di.hDC, sr, fmt.Sprintf("%d MT/s", rateOf(it.rec.Type)), sub, dtRight|dtVCenter|dtSingleLn)
		procSelectObject.Call(di.hDC, old)
	}
}

// ---------------------------------------------------------------- 控件

func (g *guiT) createControls() {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	if r, _, _ := procLoadLibraryW.Call(uintptr(unsafe.Pointer(u16ptr(richDll)))); r != 0 {
		g.richLoaded = true
	}

	g.fontUI = mkFont("Microsoft YaHei UI", S(-13), 400)
	g.fontPaint = mkFont("Microsoft YaHei UI", S(-15), 600)
	g.fontList = mkFont("Microsoft YaHei UI", S(-14), 600)
	g.fontSmall = mkFont("Microsoft YaHei UI", S(-12), 400)
	// 详情面板用新宋体：4 个西文字符正好 2 个汉字宽，寄存器列才对得齐
	g.fontMono = mkFont("新宋体", S(-13), 400)

	mkBtn := func(id uint32) uintptr {
		h, _, _ := procCreateWindowExW.Call(0,
			uintptr(unsafe.Pointer(u16ptr("BUTTON"))),
			uintptr(unsafe.Pointer(u16ptr(btnText[id]))),
			wsChild|wsVisible|wsTabStop|bsOwnerDraw,
			0, 0, 10, 10, g.hwnd, uintptr(id), hInst, 0)
		procSendMessageW.Call(h, wmSetFont, g.fontUI, 1)
		return h
	}
	g.hOpen = mkBtn(idBtnOpen)
	g.hClear = mkBtn(idBtnClear)
	g.hCopy = mkBtn(idBtnCopy)

	g.hChk, _, _ = procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16ptr("BUTTON"))),
		uintptr(unsafe.Pointer(u16ptr("按速率排序（代际标称，非实测）"))),
		wsChild|wsVisible|wsTabStop|bsAutoCheck,
		0, 0, 10, 10, g.hwnd, idChkSort, hInst, 0)
	procSendMessageW.Call(g.hChk, wmSetFont, g.fontSmall, 1)
	// 自检用：MTKEMI_GUI_SORT=1 时默认勾上“按时序排序”
	if os.Getenv("MTKEMI_GUI_SORT") != "" {
		procSendMessageW.Call(g.hChk, 0x00F1, 1, 0) // BM_SETCHECK
		g.sortSpeed = true
	}

	richStyle := uintptr(wsChild | wsVisible | wsVScroll | esMultiline | esReadOnly | esNoHideSel)
	cls := richClass
	if !g.richLoaded {
		cls = "EDIT"
	}
	g.hInfo, _, _ = procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(u16ptr(cls))), 0,
		richStyle, 0, 0, 10, 10, g.hwnd, idInfo, hInst, 0)
	g.hDet, _, _ = procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(u16ptr(cls))), 0,
		richStyle, 0, 0, 10, 10, g.hwnd, idDetail, hInst, 0)

	g.hList, _, _ = procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16ptr("LISTBOX"))), 0,
		wsChild|wsVisible|wsTabStop|wsVScroll|lbsNotify|lbsOwnerDraw|lbsNoIntegrl|lbsHasStr,
		0, 0, 10, 10, g.hwnd, idList, hInst, 0)
	procSendMessageW.Call(g.hList, wmSetFont, g.fontUI, 1)
	procSendMessageW.Call(g.hList, lbSetItemHt, 0, uintptr(S(48)))

	if g.richLoaded {
		for _, h := range []uintptr{g.hInfo, g.hDet} {
			procSendMessageW.Call(h, emSetBkColor, 1, clrCard)
			procSendMessageW.Call(h, emSetReadOnly, 1, 0)
			procSendMessageW.Call(h, emHideSelection, 0, 0)
		}
	}

	g.status = "把 preloader / boot1 拖进来，或点「选择文件…」"
	g.refreshInfo()
	g.refreshDetail()
}

func place(hw uintptr, r rectT) {
	if hw == 0 {
		return
	}
	procMoveWindow.Call(hw, uintptr(r.left), uintptr(r.top), uintptr(r.right-r.left), uintptr(r.bottom-r.top), 1)
	rgn, _, _ := procCreateRoundRgn.Call(0, 0, uintptr(r.right-r.left), uintptr(r.bottom-r.top),
		uintptr(S(20)), uintptr(S(20)))
	procSetWindowRgn.Call(hw, rgn, 1)
	fr := rectT{S(12), S(6), r.right - r.left - S(12), r.bottom - r.top - S(6)}
	procSendMessageW.Call(hw, emSetRect, 0, uintptr(unsafe.Pointer(&fr)))
}

func placePlain(hw uintptr, r rectT) {
	if hw == 0 {
		return
	}
	procMoveWindow.Call(hw, uintptr(r.left), uintptr(r.top), uintptr(r.right-r.left), uintptr(r.bottom-r.top), 1)
}

func (g *guiT) layout(w, h int32) {
	if g.hList == 0 {
		return
	}
	m := S(14)
	g.headerRect = rectT{m, S(10), w - m, S(10) + S(46)}

	by := S(66)
	bh := S(34)
	placePlain(g.hOpen, rectT{m, by, m + S(150), by + bh})
	placePlain(g.hClear, rectT{m + S(158), by, m + S(158) + S(100), by + bh})
	placePlain(g.hCopy, rectT{m + S(266), by, m + S(266) + S(120), by + bh})

	top := S(112)
	g.statusRect = rectT{m, h - S(28), w - m, h - S(6)}
	contentH := h - S(42) - top
	if contentH < S(260) {
		contentH = S(260)
	}
	gap := S(12)
	leftW := int32(float64(w-2*m-gap) * 0.45)

	// 信息卡高度跟着内容行数走：路径长短不一样，行数会变
	ih := int32(strings.Count(g.infoText(), "\n")+1)*S(26) + S(20)
	if ih < S(150) {
		ih = S(150)
	}
	if maxIH := int32(float64(contentH) * 0.56); ih > maxIH {
		ih = maxIH
	}
	g.infoRect = rectT{m, top, m + leftW, top + ih}
	g.listRect = rectT{m, g.infoRect.bottom + gap, m + leftW, top + contentH}
	g.detRect = rectT{m + leftW + gap, top, w - m, top + contentH}

	place(g.hInfo, g.infoRect)
	place(g.hDet, rectT{g.detRect.left, g.detRect.top + S(30), g.detRect.right, g.detRect.bottom})

	// 列表卡片：顶部放排序复选框，下面是列表控件
	placePlain(g.hChk, rectT{g.listRect.left + S(16), g.listRect.top + S(10),
		g.listRect.right - S(16), g.listRect.top + S(34)})
	placePlain(g.hList, rectT{g.listRect.left + S(8), g.listRect.top + S(42),
		g.listRect.right - S(8), g.listRect.bottom - S(8)})

	procSendMessageW.Call(g.hList, lbSetItemHt, 0, uintptr(S(48)))
	procInvalidateRect.Call(g.hwnd, 0, 1)
}

// ---------------------------------------------------------------- 对话框

func (g *guiT) clearAll() {
	g.files = nil
	g.items = nil
	g.sel = -1
	procSendMessageW.Call(g.hList, lbResetContent, 0, 0)
	g.refreshInfo()
	g.refreshDetail()
	g.setStatus("已清空")
}

func (g *guiT) pickFiles() {
	buf := make([]uint16, 65536)
	filter := utf16FromStr(
		"MTK 固件 (preloader/boot1/img)\x00*.bin;*.img;*.rom;*.Preloader\x00所有文件 (*.*)\x00*.*\x00\x00")
	ofn := openFileNameW{
		hwndOwner:    g.hwnd,
		lpstrFilter:  &filter[0],
		lpstrFile:    &buf[0],
		nMaxFile:     uint32(len(buf)),
		lpstrTitle:   u16ptr("选择 preloader / boot1 / PRELOADER.img（可多选）"),
		lpstrDefExt:  u16ptr("bin"),
		nFilterIndex: 1,
		flags: ofnExplorer | ofnFileMustExist | ofnPathMustExist |
			ofnAllowMultiSelect | ofnHideReadOnly | ofnNoChangeDir,
	}
	ofn.lStructSize = uint32(unsafe.Sizeof(ofn))
	ok, _, err := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	guiTrace("pickFiles -> %d err=%v", ok, err)
	if ok == 0 {
		return
	}
	parts := splitUTF16Multi(buf)
	if len(parts) == 0 {
		return
	}
	paths := parts
	if len(parts) > 1 {
		dir := parts[0]
		paths = make([]string, 0, len(parts)-1)
		for _, p := range parts[1:] {
			paths = append(paths, filepath.Join(dir, p))
		}
	}
	g.loadFiles(paths)
}

func splitUTF16Multi(buf []uint16) []string {
	var out []string
	start := 0
	for i, c := range buf {
		if c == 0 {
			if i == start {
				break
			}
			out = append(out, syscall.UTF16ToString(buf[start:i]))
			start = i + 1
		}
	}
	return out
}

func (g *guiT) copyDetail() {
	txt := g.detailText()
	if txt == "" {
		return
	}
	u := utf16FromStr(txt)
	if r, _, _ := procOpenClipboard.Call(g.hwnd); r == 0 {
		return
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	h, _, _ := procGlobalAlloc.Call(0x0042, uintptr(len(u)*2))
	if h == 0 {
		return
	}
	p, _, _ := procGlobalLock.Call(h)
	if p != 0 {
		procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)*2))
		procGlobalUnlock.Call(h)
		procSetClipboardData.Call(13, h) // CF_UNICODETEXT
	}
	g.setStatus("详情已复制到剪贴板")
}

func (g *guiT) toggleSort(on bool) {
	g.sortSpeed = on
	cur, ok := g.curItem()
	g.refreshList()
	// 勾选后尽量把原来那条重新选中
	if ok && cur.tab != nil {
		for i, it := range g.items {
			if it.tab == cur.tab && it.rec.Idx == cur.rec.Idx && it.f == cur.f {
				g.sel = i
				procSendMessageW.Call(g.hList, lbSetCurSel, uintptr(i), 0)
				g.refreshInfo()
				g.refreshDetail()
				break
			}
		}
	}
}

// ---------------------------------------------------------------- 窗口过程

var (
	childOldProc = map[uintptr]uintptr{}
	childProcAdr = syscall.NewCallback(childWndProc)
)

// 子控件（富文本、列表）自己也是拖放目标，WM_DROPFILES 会送到它们而不是主窗口，
// 这里换掉窗口过程，收到就转给主窗口。
func childWndProc(hwnd uintptr, m uint32, wParam, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 8192)
			n := runtime.Stack(buf, false)
			writeCrashLog(fmt.Sprintf("panic in childWndProc msg=0x%04X: %v\n\n%s", m, r, buf[:n]))
			ret = 0
		}
	}()
	if m == wmDropFiles {
		if g := currentGUI; g != nil && g.hwnd != 0 {
			guiTrace("child %d forwards drop", hwnd)
			procSendMessageW.Call(g.hwnd, wmDropFiles, wParam, lParam)
		}
		return 0
	}
	old := childOldProc[hwnd]
	if old == 0 {
		return 0
	}
	r, _, _ := procCallWindowProcW.Call(old, hwnd, uintptr(m), wParam, lParam)
	return r
}

func subclassChild(h uintptr) {
	if h == 0 {
		return
	}
	old, _, err := procSetWindowLongPtrW.Call(h, ^uintptr(3), childProcAdr)
	guiTrace("subclass %d -> %d err=%v", h, old, err)
	if old == 0 && err != nil && err.Error() != "The operation completed successfully." {
		return
	}
	childOldProc[h] = old
	procDragAcceptFiles.Call(h, 1)
}

func wndProc(hwnd uintptr, m uint32, wParam, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 16384)
			n := runtime.Stack(buf, false)
			writeCrashLog(fmt.Sprintf("panic in wndProc msg=0x%04X: %v\n\n%s", m, r, buf[:n]))
			ret = 0
		}
	}()
	g := currentGUI
	if g == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(m), wParam, lParam)
		return r
	}

	switch m {
	case wmCreate:
		g.hwnd = hwnd
		g.createControls()
		buildMenu(hwnd)
		updateTitle(hwnd)
		procDragAcceptFiles.Call(hwnd, 1)
		for _, h := range []uintptr{g.hInfo, g.hList, g.hDet, g.hOpen, g.hClear, g.hCopy, g.hChk} {
			subclassChild(h)
		}
		var rc rectT
		procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
		g.layout(rc.right, rc.bottom)
		return 0

	case wmSize:
		g.layout(int32(lParam&0xffff), int32((lParam>>16)&0xffff))
		procRedrawWindow.Call(hwnd, 0, 0, 0x0001|0x0004|0x0080|0x0100)
		return 0

	case wmPaint:
		g.paint(hwnd)
		return 0

	case wmEraseBkgnd:
		return 1

	case wmCtlColorStatic, wmCtlColorBtn:
		br, _, _ := procCreateSolidBrush.Call(clrCard)
		procSetBkColor.Call(wParam, clrCard)
		return br

	case wmMeasureItem:
		if lParam != 0 {
			mi := (*measureItem)(unsafe.Pointer(lParam))
			mi.itemHeight = uint32(S(48))
		}
		return 1

	case wmDrawItem:
		if lParam != 0 {
			di := (*drawItem)(unsafe.Pointer(lParam))
			switch di.CtlType {
			case odtListBox:
				g.paintListItem(di)
			case odtButton:
				g.paintButton(di)
			}
		}
		return 1

	case wmGetMinMax:
		mmi := (*minMaxInfo)(unsafe.Pointer(lParam))
		mmi.minTrackSize[0] = S(1060)
		mmi.minTrackSize[1] = S(680)
		return 0

	case wmDropFiles:
		// HDROP 在 wParam 里，lParam 是 0
		if wParam != 0 {
			defer procDragFinish.Call(wParam)
			n, _, _ := procDragQueryFileW.Call(wParam, 0xFFFFFFFF, 0, 0)
			var paths []string
			for i := 0; i < int(n); i++ {
				buf := make([]uint16, 32768)
				procDragQueryFileW.Call(wParam, uintptr(i), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
				paths = append(paths, syscall.UTF16ToString(buf))
			}
			if len(paths) > 0 {
				g.loadFiles(paths)
			}
			return 0
		}

	case wmCommand:
		id := uint32(wParam & 0xffff)
		note := uint32((wParam >> 16) & 0xffff)
		switch id {
		case idMenuOpen:
			g.pickFiles()
			return 0
		case idMenuClear:
			g.clearAll()
			return 0
		case idMenuExit:
			procDestroyWindow.Call(hwnd)
			return 0
		case idMenuAbout:
			showAbout(hwnd)
			return 0
		case idMenuDbInfo:
			g.menuDbInfo()
			return 0
		case idMenuDbEdit:
			g.menuEditDB()
			return 0
		case idMenuDbReload:
			updateDBAndRefresh(hwnd)
			return 0
		case idMenuDbWhere:
			g.menuDbWhere()
			return 0
		}
		switch id {
		case idBtnOpen:
			g.pickFiles()
			return 0
		case idBtnClear:
			g.clearAll()
			return 0
		case idBtnCopy:
			g.copyDetail()
			return 0
		case idChkSort:
			if note == bnClicked {
				r, _, _ := procSendMessageW.Call(g.hChk, 0x00F0, 0, 0) // BM_GETCHECK
				g.toggleSort(r == 1)
			}
			return 0
		case idList:
			if note == lbnSelChg {
				r, _, _ := procSendMessageW.Call(g.hList, lbGetCurSel, 0, 0)
				if int32(r) >= 0 && int(r) < len(g.items) {
					g.sel = int(r)
					g.refreshInfo()
					g.refreshDetail()
				}
			}
			return 0
		}
		return 0

	case wmAppDone:
		g.finishAsync()
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(m), wParam, lParam)
	return r
}

var wndProcAddr = syscall.NewCallback(wndProc)

// ---------------------------------------------------------------- 自检

// dumpText 把三块内容按文本导出，-guitest 用
func (g *guiT) dumpText() string {
	var b strings.Builder
	b.WriteString("=== 信息卡 ===\n")
	b.WriteString(g.infoText())
	b.WriteString("\n=== 列表 ")
	b.WriteString(strconv.Itoa(len(g.items)))
	b.WriteString(" 条 ===\n")
	for i, it := range g.items {
		l1, l2 := listRow(it)
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i, l1, l2)
	}
	b.WriteString("\n=== 详情 ===\n")
	b.WriteString(g.detailText())
	return b.String()
}

// makeHDrop 在自身进程里造一个合法 HDROP（DROPFILES + 双 NUL 结尾的宽字符路径），
// 只给 -droptest 用：跨进程 PostMessage 一个 HDROP 是无效的。
func makeHDrop(path string) uintptr {
	name := utf16FromStr(path)
	const hdr = 20
	size := hdr + len(name)*2 + 2
	h, _, _ := procGlobalAlloc.Call(0x0042, uintptr(size)) // GHND
	if h == 0 {
		return 0
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return 0
	}
	b := make([]byte, size)
	binary.LittleEndian.PutUint32(b[0:], hdr) // pFiles
	binary.LittleEndian.PutUint32(b[16:], 1)  // fWide
	for i, c := range name {
		b[hdr+i*2] = byte(c)
		b[hdr+i*2+1] = byte(c >> 8)
	}
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	procGlobalUnlock.Call(h)
	return h
}

// ---------------------------------------------------------------- 入口

func guiSupported() bool { return true }

func runGUI(args []string, selfTestOut, dropTarget string) int {
	// 窗口属于创建它的那个 OS 线程，消息循环和回调必须都在同一线程
	runtime.LockOSThread()
	initDPI()

	g := &guiT{selfTest: selfTestOut, dropTarget: dropTarget, sel: -1}
	currentGUI = g
	guiTrace("runGUI args=%v selftest=%q", args, selfTestOut)

	hInst, _, _ := procGetModuleHandleW.Call(0)
	hIcon, _, _ := procLoadIconW.Call(0, idiApp)
	hCursor, _, _ := procLoadCursorW.Call(0, idcArrow)

	wc := wndClassEx{
		style:         0x0002 | 0x0001,
		lpfnWndProc:   wndProcAddr,
		hInstance:     hInst,
		hIcon:         hIcon,
		hCursor:       hCursor,
		hbrBackground: 6, // COLOR_WINDOW+1
		lpszClassName: u16ptr(guiClass),
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		procMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16ptr("窗口类注册失败: "+err.Error()))),
			uintptr(unsafe.Pointer(u16ptr("MTKEmiParser"))), 0x10)
		return 1
	}

	hwnd, _, err := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16ptr(guiClass))),
		uintptr(unsafe.Pointer(u16ptr(appTitle))),
		wsOverlappedWindow|wsClipChildren,
		uintptr(S(120)), uintptr(S(80)), uintptr(S(1280)), uintptr(S(860)),
		0, 0, hInst, 0)
	if hwnd == 0 {
		procMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16ptr("创建窗口失败: "+err.Error()))),
			uintptr(unsafe.Pointer(u16ptr("MTKEmiParser"))), 0x10)
		return 1
	}

	if selfTestOut == "" {
		procShowWindow.Call(hwnd, swShow)
		procUpdateWindow.Call(hwnd)
		procSetFocus.Call(g.hList)
	}

	if dbTestPath != "" {
		procShowWindow.Call(hwnd, swShow)
		procUpdateWindow.Call(hwnd)
		runDBTest(g, hwnd, dbTestPath)
		procDestroyWindow.Call(hwnd)
		return 0
	}

	switch {
	case dropTarget != "" && len(args) > 0:
		target := hwnd
		if dropTarget == "edit" {
			target = g.hDet
		} else if dropTarget == "list" {
			target = g.hList
		}
		if h := makeHDrop(args[0]); h != 0 {
			guiTrace("droptest: post WM_DROPFILES(0x%X) to %s(%d)", h, dropTarget, target)
			procPostMessageW.Call(target, wmDropFiles, h, 0)
		}
	case len(args) > 0:
		g.loadFiles(args)
	}

	if selfTestOut != "" {
		var m msgT
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1)
			if r != 0 {
				procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
				procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
				continue
			}
			g.mu.Lock()
			busy, done := g.busy, g.done
			g.mu.Unlock()
			if !busy && g.hList != 0 && (dropTarget == "" || done) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		txt := g.dumpText()
		_ = os.WriteFile(selfTestOut, []byte(txt), 0o644)
		guiTrace("selftest wrote %d bytes", len(txt))
		procDestroyWindow.Call(hwnd)
		return 0
	}

	var m msgT
	for {
		r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1)
		if r == 0 {
			time.Sleep(6 * time.Millisecond)
			continue
		}
		if m.message == wmQuit {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return 0
}
