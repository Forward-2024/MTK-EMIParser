// MTKEmiParser - MTK preloader / boot1 EMI(DRAM) 配置解析器
//
// 拖入 preloader_xxx.bin、boot1.bin、PRELOADER.img 即可解析：
//   - 支持的闪存型号(eMMC CID / UFS 型号)
//   - DRAM 版本(LPDDR3/4/4X/5...) 与预置容量、rank 组成
//   - MCP / uMCP 封装形式、颗粒厂商
//   - 完整时序寄存器(EMI_CONA / DRAMC_ACTIM / DRAMC_CONF1 ...)
//   - 按 DRAM 代际速率排序
//
// 仅读取文件，不修改任何内容。
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	magicStr  = "MTK_BLOADER_INFO"
	headSize  = 112 // 表头长度：27+61+12+8+4
	minRecSz  = 96
	maxRecSz  = 400
	maxTables = 64

	// appVersion 软件版本；数据库版本在 pnmdB.tsv.gz 的 # VERSION 行里，可单独更新
	appVersion = "1.1.0"
)

// dbTestPath 只给 -dbtest 自检用（密码框 + 数据库编辑器）
var dbTestPath string

// ---------------------------------------------------------------- 数据模型

// out 是统一输出目标：控制台默认 os.Stdout，写报告时临时指向 strings.Builder，
// 这样控制台和 .emi.txt 的内容完全一致。
var out io.Writer = os.Stdout

type MRPair struct{ Reg, Val uint16 }

type Rec struct {
	Idx   int
	Sub   uint32
	Type  uint32
	IDLen uint32
	FWLen uint32
	ID    []byte
	FW    []byte
	Words []uint32 // DRAMC 寄存器
	Ranks []uint64 // 4 个 rank 容量(字节)
	MR    []MRPair // LPDDR mode register 写入对
	Valid bool
	Note  string
}

type Table struct {
	Offset    int64
	Version   int
	Suffix    string
	RawHead   string
	PreName   string
	Check     [3]uint32
	MtkBin    string
	Count     int
	RecSize   int
	Records   []Rec
	Err       string
	Trailing  uint32
	CountWant int    // 表头写的条数（被截断时和 Count 不同）
	Note      string // 非致命提示：表尾缺失、条数不足等
	Truncated bool   // 记录被截断过
}

// ---------------------------------------------------------------- 常量表

// DRAMC 寄存器名（17 字布局，v10~v29 / v32 / v39）
var fields17 = []string{
	"EMI_CONA", "DRAMC_DRVCTL0", "DRAMC_DRVCTL1", "DRAMC_ACTIM",
	"DRAMC_GDDR3CTL1", "DRAMC_CONF1", "DRAMC_DDR2CTL", "DRAMC_TEST2_3",
	"DRAMC_CONF2", "DRAMC_PD_CTRL", "DRAMC_PADCTL3", "DRAMC_DQODLY",
	"DRAMC_ADDR_OUTPUT_DLY", "DRAMC_CLK_OUTPUT_DLY", "DRAMC_ACTIM1",
	"DRAMC_MISCTL0", "DRAMC_ACTIM05T",
}

// 10 字布局（v30/v31/v35/v36/v38/v39/v51）
var fields10 = []string{
	"EMI_CONA", "EMI_CONH", "DRAMC_ACTIME0", "DRAMC_ACTIME1",
	"DRAMC_ACTIME2", "DRAMC_ACTIME3", "DRAMC_ACTIME4", "DRAMC_ACTIME5",
	"DRAMC_ACTIME6", "DRAMC_ACTIME7",
}

// 介质类型 + DDR 代际（m_type 字段）
// 高半字节：0=分离 1=NAND MCP 2=eMCP 3=uMCP
// 低字节  ：1=DDR1 2=LPDDR2 3=LPDDR3 4=PCDDR3 5=LPDDR4 6=LPDDR4X 7=LPDDR5 8=LPDDR5X
var ddrName = map[uint32]string{
	1: "DDR1", 2: "LPDDR2", 3: "LPDDR3", 4: "PCDDR3",
	5: "LPDDR4", 6: "LPDDR4X", 7: "LPDDR5", 8: "LPDDR5X",
}

// 代际 -> 速率。注意：EMI 表里【没有】实测频率字段，这里给的是该代际的 JEDEC 常见速率，
// 只能当“代际标称值”看，不是这颗料真正的运行频率。
// 之前这张表填的其实是**时钟 MHz**（933 / 1600 / 1866 / 3200 …），却按 MT/s 显示，
// 等于把所有 LPDDR3/4/4X 的速率都少写了一倍。MT/s = MHz × 2。
var ddrRateMTS = map[uint32]int{
	1: 400,  // DDR-400
	2: 1066, // LPDDR2-1066
	3: 1866, // LPDDR3-1866
	4: 1066, // DDR3-1066
	5: 3200, // LPDDR4-3200
	6: 3733, // LPDDR4X-3733
	7: 6400, // LPDDR5-6400
	8: 8533, // LPDDR5X-8533
}

// rateOf 返回代际标称数据率（MT/s）
func rateOf(typ uint32) int { return ddrRateMTS[ddrGen(typ)] }

// clockOf 返回代际标称时钟（MHz）= MT/s / 2
func clockOf(typ uint32) int { return rateOf(typ) / 2 }

// rateText 统一成 “3733 MT/s (1866 MHz)”
func rateText(typ uint32) string {
	if r := rateOf(typ); r > 0 {
		return fmt.Sprintf("%d MT/s (%d MHz)", r, r/2)
	}
	return "—"
}

// eMMC CID 的 MID（厂商号）。按 JEDEC MMCA 注册表 + 实测样本整理，
// 并参照了 emmcinfo 那套工具的表（0xF4 佰维、0xD6 江波龙、0x88 长江存储…）。
// 注意 0x88 在部分资料里写 Micron，实测样机多为 YMTC 颗粒，这里按 YMTC 标。
var midVendor = map[byte]string{
	0x01: "Panasonic",
	0x02: "SanDisk",
	0x03: "SanDisk",
	0x04: "ISOCOM",
	0x11: "Toshiba/Kioxia",
	0x13: "Micron",
	0x15: "Samsung",
	0x2C: "Micron",
	0x45: "SanDisk/WD",
	0x70: "Kingston",
	0x88: "YMTC/长江存储",
	0x90: "SK Hynix",
	0xD6: "Longsys/江波龙",
	0xDF: "SCY/Shichuangyi",
	0xEA: "SiliconGo/硅格",
	0xF4: "BIWIN/佰维",
	0xFE: "Micron/Numonyx",
}

// emmcSpec：CID 里的 PRV 字节 → eMMC 规范版本（和 emmcinfo 工具的对照表一致）
func emmcSpec(prv byte) string {
	switch prv & 0x0F {
	case 5:
		return "eMMC 4.41"
	case 6:
		return "eMMC 4.51"
	case 7:
		return "eMMC 5.0"
	case 8:
		return "eMMC 5.1"
	}
	return ""
}

// ---------------------------------------------------------------- 入口

type Options struct {
	Sort    string
	Verbose bool
	JSON    bool
	CSV     bool
	NoOut   bool
	NoPause bool
	OutDir  string

	// QuietReport 为真时不往输出里打“报告已写入: ...”，GUI 自己安排在末尾显示
	QuietReport bool
}

func main() {
	opts := Options{}
	flag.StringVar(&opts.Sort, "sort", "speed", "排序方式：speed(速率优先) | cap(容量优先) | idx(原始顺序) | type")
	flag.BoolVar(&opts.Verbose, "v", false, "显示完整时序寄存器与 MR 表")
	flag.BoolVar(&opts.JSON, "json", false, "输出 JSON 到标准输出")
	flag.BoolVar(&opts.CSV, "csv", false, "额外输出 <文件>.emi.csv")
	flag.BoolVar(&opts.NoOut, "noout", false, "不写 <文件>.emi.txt 报告")
	flag.BoolVar(&opts.NoPause, "nopause", false, "结束时不等待按键")
	flag.StringVar(&opts.OutDir, "outdir", "", "报告输出目录（默认 exe 所在目录；same = 与源文件同目录）")
	guiTest := flag.String("guitest", "", "内部自检：不显示窗口，把结果框内容写入指定文件后退出")
	dropTest := flag.String("droptest", "", "内部自检：配 -guitest，把第一个文件投递到 edit / main，验证拖放链路")
	dbTest := flag.String("dbtest", "", "内部自检：跑一遍密码框 + 数据库编辑器，把结果写入指定文件后退出")
	flag.Usage = usage
	flag.Parse()

	initConsole()
	dbTestPath = *dbTest

	// 不带任何开关时走图形界面：双击 exe、或把文件拖到 exe 图标上都进窗口。
	// 一旦带了开关（-v / -json / -csv / -outdir ...），就走原来的命令行模式。
	if *guiTest != "" && guiSupported() {
		os.Exit(runGUI(flag.Args(), *guiTest, *dropTest))
	}
	if dbTestPath != "" && guiSupported() {
		os.Exit(runGUI(nil, "", ""))
	}
	if flag.NFlag() == 0 && guiSupported() {
		os.Exit(runGUI(flag.Args(), "", ""))
	}

	files := flag.Args()
	if len(files) == 0 {
		usage()
		pause(opts.NoPause)
		return
	}

	var all []*FileResult
	for _, f := range files {
		fr := processFile(f, opts)
		all = append(all, fr)
	}

	if opts.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(buildJSON(all, opts))
		return
	}

	for _, fr := range all {
		renderFile(fr, opts)
	}
	pause(opts.NoPause)
}

func usage() {
	fmt.Fprint(out, `
MTKEmiParser v1.0  —— MTK preloader / boot1 EMI 配置解析

用法：
    MTKEmiParser.exe <preloader.bin | boot1.bin | PRELOADER.img> [...]

参数：
    -sort speed|cap|idx|type   表内记录排序方式（默认 speed：DDR 速率优先）
    -v                         显示完整时序寄存器 + LPDDR MR 表
    -json                      输出 JSON
    -csv                       额外导出 <文件>.emi.csv
    -noout                     不生成 <文件>.emi.txt
    -outdir <目录>             报告输出目录（默认 exe 所在目录；same = 与源文件同目录）
    -nopause                   结束不等待按键

说明：
    直接把文件拖到 exe 上即可。支持 preloader_xxx.bin、
    eMMC boot1 分区镜像（boot1.bin）、整块 PRELOADER.img 读回。
    文件里有多张 MTK_BLOADER_INFO 表时会全部列出。
`)
}

// ---------------------------------------------------------------- 平台(SoC)识别

// platCode：MTK preloader 的项目代号 → SoC 型号。只收实测见过的代号，
// 拿不准的写成两个候选。识别不出来就老实说识别不出来，不瞎猜。
var platCode = map[string]string{
	"k37mv1": "MT6735/MT6737",
	"k50v1":  "MT6755",
	"k53":    "MT6753",
	"k61v1":  "MT6761",
	"k62v1":  "MT6762/MT6765",
	"k63v2":  "MT6763",
	"k71v1":  "MT6771",
	"k80":    "MT6580",
}

// PlatformInfo 是“推测”出来的平台信息，永远带依据和置信度。
type PlatformInfo struct {
	Name  string `json:"name,omitempty"`       // MT6761
	Code  string `json:"code,omitempty"`       // k61v1（项目代号）
	Basis string `json:"basis,omitempty"`      // 依据
	Conf  string `json:"confidence,omitempty"` // 较可信 / 推测 / 未知
}

// socFromData：在 preloader 二进制里找 MTxxxx 型号串。
// MT63xx 是 PMIC、MT66xx 是 WiFi/BT/FM 组合芯片，都不是 SoC，要排掉；
// 另外 "MT6752" 这个串几乎每份 preloader 里都出现一次（公共库残留），
// 只出现一次时不能当证据，否则所有机器都会被认成 MT6752。
func socFromData(data []byte) (string, int) {
	cnt := map[string]int{}
	limit := len(data)
	if limit > 96<<20 {
		limit = 96 << 20
	}
	for i := 0; i+6 <= limit; i++ {
		c := data[i]
		if c != 'M' && c != 'm' {
			continue
		}
		if data[i+1] != 'T' && data[i+1] != 't' {
			continue
		}
		ok := true
		for k := 2; k < 6; k++ {
			if data[i+k] < '0' || data[i+k] > '9' {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		digits := string(data[i+2 : i+6])
		if strings.HasPrefix(digits, "63") || strings.HasPrefix(digits, "66") {
			continue
		}
		cnt["MT"+digits]++
	}
	best, bestN := "", 0
	for k, v := range cnt {
		if v > bestN || (v == bestN && k < best) {
			best, bestN = k, v
		}
	}
	return best, bestN
}

// platTokens 从文件名里挑出 k61v1 / k80 / k37mv1 这类项目代号
func platTokens(name string) []string {
	l := strings.ToLower(name)
	var out []string
	for _, tok := range strings.FieldsFunc(l, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z'))
	}) {
		if len(tok) < 3 || len(tok) > 8 || tok[0] != 'k' {
			continue
		}
		digits := 0
		for i := 1; i < len(tok) && tok[i] >= '0' && tok[i] <= '9'; i++ {
			digits++
		}
		if digits >= 2 {
			out = append(out, tok)
		}
	}
	return out
}

// socInName：文件名里直接带 SoC 数字的（tb8735ma1_bsp → MT8735、u2_6761vwe → MT6761）
func socInName(name string) string {
	l := strings.ToLower(name)
	for i := 0; i+4 <= len(l); i++ {
		if l[i] < '0' || l[i] > '9' {
			continue
		}
		allDigit := true
		for k := 0; k < 4; k++ {
			if l[i+k] < '0' || l[i+k] > '9' {
				allDigit = false
				break
			}
		}
		if !allDigit {
			continue
		}
		d := l[i : i+4]
		if strings.HasPrefix(d, "67") || strings.HasPrefix(d, "68") || strings.HasPrefix(d, "69") ||
			strings.HasPrefix(d, "65") || strings.HasPrefix(d, "87") || strings.HasPrefix(d, "81") {
			return "MT" + d
		}
	}
	return ""
}

func guessPlatform(data []byte, preName, fileName string) PlatformInfo {
	// 1) 文件里直接出现 SoC 型号串 —— 最硬的证据
	soc, n := socFromData(data)
	if soc != "" && !(soc == "MT6752" && n < 4) {
		conf := "推测"
		if n >= 2 {
			conf = "较可信"
		}
		return PlatformInfo{Name: soc, Basis: fmt.Sprintf("文件里出现 %d 次 “%s”", n, soc), Conf: conf}
	}
	hint := ""
	if soc == "MT6752" {
		hint = fmt.Sprintf("文件里只出现 %d 处 “MT6752”，这是公共库里常见的残留串，不足以判断", n)
	}
	// 2) 文件名/preloader 名里带 4 位 SoC 数字
	for _, n := range []string{preName, fileName} {
		if n == "" {
			continue
		}
		if soc := socInName(n); soc != "" {
			return PlatformInfo{Name: soc, Basis: "按名字里的型号数字 " + soc[2:] + " 推测", Conf: "推测"}
		}
	}
	// 3) 项目代号对照表
	for _, n := range []string{preName, fileName} {
		if n == "" {
			continue
		}
		toks := platTokens(n)
		for _, tok := range toks {
			if soc, ok := platCode[tok]; ok {
				return PlatformInfo{Name: soc, Code: tok, Basis: "按项目代号 " + tok + " 推测", Conf: "推测"}
			}
		}
		for _, tok := range toks {
			return PlatformInfo{Code: tok, Basis: "项目代号 " + tok + "（不在对照表里）", Conf: "未知"}
		}
	}
	if hint != "" {
		return PlatformInfo{Basis: hint, Conf: "未知"}
	}
	return PlatformInfo{}
}

// platDisplay 把 PlatformInfo 变成界面上那一行文字
func platDisplay(p PlatformInfo) string {
	switch {
	case p.Name != "" && p.Conf == "较可信":
		return fmt.Sprintf("%s  ·  %s，较可信", p.Name, p.Basis)
	case p.Name != "":
		return fmt.Sprintf("%s  ·  %s，不一定准", p.Name, p.Basis)
	case p.Code != "":
		return fmt.Sprintf("未识别  ·  %s，不一定准", p.Basis)
	default:
		return "未识别  ·  文件和文件名里都没有可用的平台标识"
	}
}

// ---------------------------------------------------------------- 文件处理

type FileResult struct {
	Path    string   `json:"path"`
	Size    int64    `json:"size"`
	SizeHex string   `json:"size_hex"`
	Tables  []*Table `json:"tables"`
	Skipped int      `json:"-"`
	Error   string   `json:"error,omitempty"`

	// 平台是猜的，永远带依据（GUI/报告/JSON 都会原样带出“不一定准”）
	Platform PlatformInfo `json:"platform"`
}

func processFile(path string, opts Options) *FileResult {
	fr := &FileResult{Path: path}
	st, err := os.Stat(path)
	if err != nil {
		fr.Error = err.Error()
		return fr
	}
	fr.Size = st.Size()
	fr.SizeHex = fmt.Sprintf("0x%X", st.Size())

	if st.IsDir() {
		fr.Error = "这是文件夹，不是文件。把里面的 preloader_xxx.bin / boot1.bin 拖进来（或者直接拖整个设备目录，会自动找）"
		return fr
	}

	data, err := os.ReadFile(path)
	if err != nil {
		fr.Error = err.Error()
		return fr
	}
	if st.Size() > 128<<20 {
		// 超大镜像（整片 eMMC 读回）也能处理，但提示一下
	}

	if os.Getenv("MTKEMI_DEBUG") != "" {
		fmt.Fprintf(out, "[dbg] len=%d first=%d\n", len(data), bytes.Index(data, []byte(magicStr)))
	}
	fr.Tables, fr.Skipped = findAllTables(data)
	preName := ""
	for _, t := range fr.Tables {
		if t.PreName != "" {
			preName = t.PreName
			break
		}
	}
	fr.Platform = guessPlatform(data, preName, filepath.Base(path))
	if len(fr.Tables) == 0 {
		switch {
		case len(data) == 0:
			fr.Error = "文件为空（0 字节），可能拷贝失败或文件被占用"
		case len(data) < headSize:
			fr.Error = "文件太小，不可能是 preloader"
		case !bytes.Contains(data, []byte(magicStr)):
			fr.Error = "文件里没有 MTK_BLOADER_INFO 标记：不是 MTK 平台的 preloader/boot1" +
				"（展锐/瑞芯微/全志等平台的固件没有这张表）"
		case fr.Skipped > 0:
			fr.Error = fmt.Sprintf("命中 %d 处 MTK_BLOADER_INFO 标记，但记录全部不合法（残留数据或被截断的副本）", fr.Skipped)
		default:
			fr.Error = "有 MTK_BLOADER_INFO 标记，但表头结构对不上（可能是被裁掉一部分或加密的 preloader）"
		}
	}
	if !opts.NoOut && len(fr.Tables) > 0 && !opts.JSON && !opts.CSV {
		writeReport(path, fr, opts)
	}
	return fr
}

func findAllTables(data []byte) ([]*Table, int) {
	var out []*Table
	skipped := 0
	pos := 0
	seen := map[int64]bool{}
	for len(out) < maxTables {
		i := bytes.Index(data[pos:], []byte(magicStr))
		if i < 0 {
			break
		}
		off := int64(pos + i)
		pos = int(pos+i) + 1
		if seen[off] {
			continue
		}
		seen[off] = true
		if t := parseTable(data, off); t != nil {
			if badTable(t) {
				skipped++
				continue
			}
			out = append(out, t)
		}
	}
	return out, skipped
}

// badTable：整表记录基本都不合法 → 说明这个 MTK_BLOADER_INFO 标记是误命中
// （镜像里残留的旧数据、压缩块、被截断的副本等），直接丢掉免得污染结果。
func badTable(t *Table) bool {
	if t.Count == 0 || len(t.Records) == 0 {
		return false
	}
	bad := 0
	for _, r := range t.Records {
		if !r.Valid {
			bad++
		}
	}
	return bad*2 >= len(t.Records)
}

func u32(b []byte, o int) uint32 { return binary.LittleEndian.Uint32(b[o:]) }
func u64(b []byte, o int) uint64 { return binary.LittleEndian.Uint64(b[o:]) }

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

func parseTable(data []byte, off int64) *Table {
	o := int(off)
	if o+headSize > len(data) {
		return nil
	}
	t := &Table{Offset: off}
	raw := data[o : o+27]
	t.RawHead = cstr(raw)
	if !strings.HasPrefix(t.RawHead, magicStr) {
		return nil
	}
	// 版本号：MTK_BLOADER_INFO_v35_EMMC / MTK_BLOADER_INFO_v20
	rest := strings.TrimPrefix(t.RawHead[len(magicStr):], "_")
	if len(rest) < 3 || rest[0] != 'v' {
		return nil
	}
	t.Version, _ = strconv.Atoi(rest[1:3])
	if i := strings.IndexByte(rest, '_'); i >= 0 {
		t.Suffix = rest[i+1:]
	}
	t.PreName = cstr(data[o+27 : o+27+61])
	for i := 0; i < 3; i++ {
		t.Check[i] = u32(data, o+88+i*4)
	}
	t.MtkBin = cstr(data[o+100 : o+100+8])
	t.Count = int(u32(data, o+108))
	if t.Count < 0 || t.Count > 4096 { // 上界放宽：截断/损坏的表条数可能是垃圾值，后面会按实际数据量夹紧
		return nil
	}

	base := o + headSize
	if t.Count == 0 {
		t.RecSize = 0
		return t
	}
	if base >= len(data) {
		t.Err = "表头之后没有任何记录数据（表头是完整的，记录区丢了）"
		return t
	}

	// 表头写的条数超过文件里能容纳的条数 → 一定是被截断了，
	// 先按“还能放下多少条”算，别整张表丢掉。
	maxPossible := (len(data) - base) / minRecSz
	if maxPossible < 1 {
		t.Err = "表头之后没有一条完整的记录（文件被截断在表头后面）"
		return t
	}
	if t.Count > maxPossible {
		t.CountWant = t.Count
		t.Count = maxPossible
		t.Truncated = true
	}

	rec, trailing := detectRecSize(data, base, t.Count)
	if rec == 0 || trailing == 0 {
		// 尾部那串“长度字段”缺失时（表被单独截出来、或文件被裁过），
		// detectRecSize 按 4 字节步进盲猜宽度很容易猜错，
		// 换成“版本默认宽度 + 逐条记录合法性”再挑一次。
		if r2 := pickRecSize(data, base, t.Count, t.Version); r2 > 0 {
			rec = r2
		}
		if rec > 0 && trailing == 0 {
			addNote(t, fmt.Sprintf("表尾缺少记录长度字段（可能被截断或单独提取），按 v%d 常见宽度 %d 字节/条解析",
				t.Version, rec))
		}
	}
	if rec == 0 {
		if def := defaultRecSize(t.Version); def > 0 && len(data)-base < def {
			t.Err = fmt.Sprintf("记录区只剩 %d 字节，不够一条 v%d 记录（%d 字节），文件被截断在记录中间",
				len(data)-base, t.Version, def)
		} else {
			t.Err = "记录长度无法确定（表结构异常）"
		}
		return t
	}
	t.RecSize = rec
	t.Trailing = trailing

	if fit := (len(data) - base) / rec; fit < t.Count {
		if t.CountWant == 0 {
			t.CountWant = t.Count
		}
		t.Count = fit
		t.Truncated = true
	}
	if fit := (len(data) - base) / rec; fit == 0 {
		t.Err = fmt.Sprintf("表头之后只剩 %d 字节，不够一条 %d 字节的记录", len(data)-base, rec)
		return t
	}
	if t.Truncated {
		addNote(t, fmt.Sprintf("文件被截断：表头写 %d 条，只解析到 %d 条完整记录", t.CountWant, t.Count))
	}

	for i := 0; i < t.Count; i++ {
		s := base + i*rec
		if s+rec > len(data) {
			t.Err = fmt.Sprintf("记录 %d 越界", i)
			break
		}
		t.Records = append(t.Records, parseRecord(data[s:s+rec], i, t.Version, rec))
	}
	return t
}

func addNote(t *Table, s string) {
	if t.Note != "" {
		t.Note += "；"
	}
	t.Note += s
}

// 各版本记录的默认长度 = emi_structures.h 里 emi_len[N] × 4 字节。
// 只有在表尾长度字段缺失（截断的表）时才用它当优先候选。
var defaultRecSizes = map[int]int{
	8: 136, 10: 184, 11: 168, 12: 188, 13: 188, 14: 168, 15: 176,
	16: 188, 17: 176, 18: 188, 19: 176, 20: 188, 21: 188, 22: 176,
	23: 152, 24: 176, 25: 160, 27: 188, 28: 148, 30: 160, 31: 160,
	32: 188, 35: 160, 36: 160, 38: 160, 39: 160, 46: 88, 49: 84, 51: 112,
}

func defaultRecSize(ver int) int { return defaultRecSizes[ver] }

// pickRecSize：没有尾部长度字段可用时挑记录宽度。
// 打分 = 每条记录合法性(±20/-5) + 版本默认宽度加成，
// 这样 160 和 164 这种相邻宽度不会互相打架（默认宽度优先）。
func pickRecSize(data []byte, base, count, ver int) int {
	avail := len(data) - base
	if count <= 0 || avail <= 0 {
		return 0
	}
	cands := []int{}
	if def := defaultRecSize(ver); def > 0 {
		cands = append(cands, def)
	}
	for rec := minRecSz; rec <= maxRecSz; rec += 4 {
		if rec != defaultRecSize(ver) {
			cands = append(cands, rec)
		}
	}
	best, bestScore := 0, -1
	for _, rec := range cands {
		fit := avail / rec
		if fit < 1 {
			continue
		}
		if fit > count {
			fit = count
		}
		sc := 0
		if rec == defaultRecSize(ver) {
			sc += 12
		}
		for i := 0; i < fit && i < 32; i++ {
			if parseRecord(data[base+i*rec:base+(i+1)*rec], i, ver, rec).Valid {
				sc += 20
			} else {
				sc -= 5
			}
		}
		if sc > bestScore {
			best, bestScore = rec, sc
		}
	}
	if bestScore <= 0 {
		return 0
	}
	return best
}

// detectRecSize：用“尾部长度字段 == 112+count*rec”与“记录内容合法性”双重打分
func detectRecSize(data []byte, base, count int) (int, uint32) {
	bestRec, bestScore := 0, -1
	var bestTrailing uint32
	for rec := minRecSz; rec <= maxRecSz; rec += 4 {
		end := base + count*rec
		if end+4 > len(data) {
			break
		}
		score := 0
		var trailing uint32
		want := uint32(headSize + count*rec)
		if u32(data, end) == want {
			score += 100
			trailing = want
		}
		okRec, checked := 0, 0
		for i := 0; i < count && i < 16; i++ {
			s := base + i*rec
			typ := u32(data, s+4)
			idl := u32(data, s+8)
			checked++
			if typ <= 0x400 && idl <= 16 {
				okRec++
			}
		}
		if checked > 0 {
			score += okRec * 10 / checked
		}
		if score > bestScore {
			bestScore, bestRec, bestTrailing = score, rec, trailing
		}
	}
	if bestScore <= 0 {
		return 0, 0
	}
	return bestRec, bestTrailing
}

// parseRecord 兼容三种布局：
//
//	A: 通用 eMMC 记录  [头16][ID16][FW8][DRAMC words][rank 4x u32][保留][MR 24B]
//	B: v46/v49 UFS 记录 [type][idlen][...][ufs_id16][rank 4x u64]
//	C: v51 记录        [sub][type][idlen][fwlen][id16][fw8][cona][conh][8 words][rank 4x u64]
func parseRecord(b []byte, idx, ver, recSz int) Rec {
	r := Rec{Idx: idx}

	isUFS := ver == 46 || ver == 49 || ver == 51

	switch {
	case ver == 11 && recSz >= 168:
		// v11 没有 sub_ver/fw_id：[type][ID16][idlen][dramc0 30 words][rank 4x u32]
		r.Type = u32(b, 0)
		r.ID = clone(b[4:20])
		r.IDLen = u32(b, 20)
		r.Words = words(b, 24, 30)
		r.Ranks = dwords(b, 144)
	case ver == 49 && recSz >= 56:
		r.Type = u32(b, 0)
		r.IDLen = u32(b, 4)
		r.ID = clone(b[8:24])
		r.Ranks = qwords(b, 24)
	case ver == 46 && recSz >= 88:
		r.Type = u32(b, 0)
		r.IDLen = u32(b, 4)
		r.ID = clone(b[40:56])
		r.Ranks = qwords(b, 56)
	case ver == 51 && recSz >= 112:
		r.Sub = u32(b, 0)
		r.Type = u32(b, 4)
		r.IDLen = u32(b, 8)
		r.FWLen = u32(b, 12)
		r.ID = clone(b[16:32])
		r.FW = clone(b[32:40])
		r.Words = words(b, 40, 10)
		r.Ranks = qwords(b, 80)
	default:
		r.Sub = u32(b, 0)
		r.Type = u32(b, 4)
		r.IDLen = u32(b, 8)
		r.FWLen = u32(b, 12)
		r.ID = clone(b[16:32])
		r.FW = clone(b[32:40])

		// v30 / v31 / v35 / v36 / v38 / v39 的 m_dram_rank_size[4] 是 qlong（8 字节），
		// 其余版本是 quint（4 字节）。按 4 字节读会把 4GB 的 rank 截成高位丢失的 0。
		// 依据：MTK emi_structures.h 里 EMIInfoV35/V36/V38/V39 的字段类型。
		rankW := 4
		switch ver {
		case 30, 31, 35, 36, 38, 39:
			rankW = 8
		}

		// 记录布局其实有三种，靠版本号猜不可靠（同一个 v25 在不同 SoC 上宽度都不一样）：
		//   A. 17 个寄存器字 + rank×4(u32) + 64 字节尾巴（v11~v28/v32，188 字节）
		//   B. 10 个寄存器字 + rank×4(u64/u32) @80 + 尾巴（v30+/160 字节，也见 v25/144）
		// 顺序：先按版本默认，rank 数据不合理就换下一种，最后兜底回默认。
		type lay struct {
			wordN   int
			rankOff int
			rankW   int
			mr      bool
		}
		cands := []lay{
			{wordN: (recSz - 120) / 4, rankOff: recSz - 80, rankW: rankW, mr: rankW == 4},
			{wordN: 10, rankOff: 80, rankW: 4},
			{wordN: 10, rankOff: 80, rankW: 8},
		}
		chosen, picked := cands[0], false
		for _, c := range cands {
			if c.wordN < 1 || 40+c.wordN*4 > len(b) || c.rankOff+4*c.rankW > len(b) {
				continue
			}
			if ranksLookOK(pickRanks(b, c.rankOff, c.rankW)) {
				chosen, picked = c, true
				break
			}
		}
		if !picked {
			chosen = cands[0]
		}
		if chosen.wordN < 1 {
			chosen.wordN = 1
		}
		r.Words = words(b, 40, chosen.wordN)
		if chosen.rankW == 8 {
			r.Ranks = qwords(b, chosen.rankOff)
		} else {
			r.Ranks = dwords(b, chosen.rankOff)
		}
		if chosen.mr && recSz >= 24 {
			r.MR = mrPairs(b[recSz-24:])
		}
	}

	if r.IDLen > 16 {
		r.IDLen = 16
	}
	// 合法性判断
	r.Valid = true
	note := func(s string) {
		if r.Note != "" {
			r.Note += "；"
		}
		r.Note += s
		r.Valid = false
	}
	if r.Type > 0x3FF {
		note(fmt.Sprintf("type=0x%X 超出已知范围", r.Type))
	} else {
		gen, pkg := r.Type&0xFF, r.Type>>8
		if gen < 1 || gen > 8 {
			note(fmt.Sprintf("DDR 代际字节 0x%X 非法", gen))
		}
		if pkg > 3 {
			note(fmt.Sprintf("封装字节 0x%X 非法", pkg))
		}
	}
	for _, rk := range r.Ranks {
		if rk < 1<<20 { // 0 或小标志位（部分 preloader 在 rank[3] 里放 1）
			continue
		}
		if rk%(128<<20) != 0 || rk > 32<<30 {
			note(fmt.Sprintf("rank 容量 0x%X 异常", rk))
			break
		}
	}
	if isUFS {
		r.MR = nil
	}
	return r
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

func words(b []byte, off, n int) []uint32 {
	if n < 0 {
		return nil
	}
	if off+n*4 > len(b) {
		n = (len(b) - off) / 4
	}
	out := make([]uint32, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, u32(b, off+i*4))
	}
	return out
}

func dwords(b []byte, off int) []uint64 {
	out := make([]uint64, 0, 4)
	for i := 0; i < 4; i++ {
		if off+i*4+4 > len(b) {
			out = append(out, 0)
			continue
		}
		out = append(out, uint64(u32(b, off+i*4)))
	}
	return out
}

func qwords(b []byte, off int) []uint64 {
	out := make([]uint64, 0, 4)
	for i := 0; i < 4; i++ {
		if off+i*8+8 > len(b) {
			out = append(out, 0)
			continue
		}
		out = append(out, u64(b, off+i*8))
	}
	return out
}

// pickRanks 按宽度取 4 个 rank 容量
func pickRanks(b []byte, off, width int) []uint64 {
	if width == 8 {
		return qwords(b, off)
	}
	return dwords(b, off)
}

// ranksLookOK：4 个 rank 要么是 0，要么是 128MB 的整数倍（≤32GB）。
// 全 0 是合法的（通配记录，容量由运行时自检报），但只要出现不是 128MB 倍数的值，
// 就说明这个偏移/宽度读到的不是 rank 数组。
func ranksLookOK(v []uint64) bool {
	for _, x := range v {
		if x == 0 {
			continue
		}
		if x%(128<<20) != 0 || x > 32<<30 {
			return false
		}
	}
	return true
}

// mrPairs：末尾 24 字节 = 6 个 u32，每个 u32 打包成 (MR号, 值) 两个 u16
func mrPairs(b []byte) []MRPair {
	if len(b) < 24 {
		return nil
	}
	out := make([]MRPair, 0, 6)
	for i := 0; i < 6; i++ {
		reg := binary.LittleEndian.Uint16(b[i*4:])
		val := binary.LittleEndian.Uint16(b[i*4+2:])
		if reg == 0 && val == 0 {
			continue
		}
		if reg > 0x3F {
			continue
		}
		out = append(out, MRPair{reg, val})
	}
	return out
}

// ---------------------------------------------------------------- 语义解码

func ddrGen(typ uint32) uint32 { return typ & 0xFF }

func ddrKind(typ uint32) string {
	g := ddrGen(typ)
	if n, ok := ddrName[g]; ok {
		return n
	}
	return fmt.Sprintf("未知(0x%X)", g)
}

func packKind(typ uint32) string {
	switch typ >> 8 {
	case 0:
		return "分离式"
	case 1:
		return "NAND-MCP"
	case 2:
		return "eMCP"
	case 3:
		return "uMCP"
	}
	return "—"
}

// mediaKind 判断闪存介质。
//
// 关键：eMMC 和 UFS 在 EMI 表里的 ID 完全不是一回事——
//   - eMMC  存的是 9~16 字节二进制 CID（首字节是 MID，0x15=Samsung、0x90=SK Hynix…）
//   - UFS/uMCP 存的是 ASCII 料号（"KM2H7001CM-B518"、"H9HQ15AECMBDAR"…）
//
// 所以先用 ID 的形态判断，形态定不了（idlen=0 的通配记录）才退回封装字节：
// 3 = uMCP 本身就是 UFS+LPDDR 的组合。注意三星同名 uMCP 里也有 eMMC 版（KM3P/KM4X），
// 那种记录里的 ID 会是二进制 CID，会被上面的规则正确判成 eMMC。
func mediaKind(r Rec, suffix string) string {
	if strings.Contains(strings.ToUpper(suffix), "UFS") {
		return "UFS"
	}
	pkg := r.Type >> 8
	if pkg == 1 {
		return "NAND"
	}
	u := usedID(r.ID, r.IDLen)
	if len(u) > 0 {
		if isASCIIPart(u) {
			return "UFS"
		}
		return "eMMC"
	}
	// 通配记录（idlen=0，不绑型号）：uMCP 按 UFS 计
	if pkg == 3 {
		return "UFS"
	}
	return "eMMC"
}

func usedID(id []byte, idLen uint32) []byte {
	n := int(idLen)
	if n > len(id) {
		n = len(id)
	}
	if idLen == 0 {
		n = 0
	}
	return id[:n]
}

func isASCIIPart(id []byte) bool {
	if len(id) < 6 {
		return false
	}
	if id[0] < 0x20 || id[0] > 0x7E {
		return false
	}
	n := 0
	for _, c := range id {
		if c == 0 {
			continue
		}
		if c < 0x20 || c > 0x7E {
			return false
		}
		n++
	}
	return n >= 6
}

func trimNul(b []byte) []byte {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return b[:i]
	}
	return b
}

func totalCap(ranks []uint64) uint64 {
	var s uint64
	for _, r := range ranks {
		if r >= 1<<20 {
			s += r
		}
	}
	return s
}

func humanSize(v uint64) string {
	if v == 0 {
		return "—"
	}
	const k = 1024.0
	f := float64(v)
	switch {
	case v >= 1<<30:
		return trimFloat(f/(k*k*k)) + "GB"
	case v >= 1<<20:
		return trimFloat(f/(k*k)) + "MB"
	case v >= 1<<10:
		return trimFloat(f/k) + "KB"
	}
	return fmt.Sprintf("%dB", v)
}

func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
}

func composition(ranks []uint64) string {
	var parts []uint64
	for _, r := range ranks {
		if r >= 1<<20 {
			parts = append(parts, r)
		}
	}
	if len(parts) == 0 {
		return "运行时自检"
	}
	same := true
	for _, p := range parts[1:] {
		if p != parts[0] {
			same = false
			break
		}
	}
	if same {
		return humanSize(parts[0]) + "×" + strconv.Itoa(len(parts))
	}
	var ss []string
	for _, p := range parts {
		ss = append(ss, humanSize(p))
	}
	return strings.Join(ss, "+")
}

func vendorOf(id []byte, idLen uint32) string {
	u := usedID(id, idLen)
	if len(u) == 0 {
		return "通配(不限型号)"
	}
	if isASCIIPart(u) {
		// uMCP/UFS：ID 直接就是料号
		pn := strings.ToUpper(string(trimNul(u)))
		switch {
		case strings.HasPrefix(pn, "KM"):
			return "Samsung"
		case strings.HasPrefix(pn, "H9"), strings.HasPrefix(pn, "H8"), strings.HasPrefix(pn, "H2"):
			return "SK Hynix"
		case strings.HasPrefix(pn, "MT"), strings.HasPrefix(pn, "MZ"):
			return "Micron"
		case strings.HasPrefix(pn, "TH"), strings.HasPrefix(pn, "TC"):
			return "Toshiba/Kioxia"
		case strings.HasPrefix(pn, "SD"), strings.HasPrefix(pn, "S0"):
			return "SanDisk"
		}
		return "—"
	}
	// 部分 UFS preloader 的 m_ufs_id 前 2 字节就是 UFS 厂商号（emi_structures.h 的 UFS_VENDOR_*）
	if len(u) >= 2 {
		if v, ok := ufsVendorID[uint16(u[0])|uint16(u[1])<<8]; ok {
			return v
		}
	}
	if v, ok := midVendor[u[0]]; ok {
		return v
	}
	return fmt.Sprintf("未知(0x%02X)", u[0])
}

// UFS 厂商号，见 emi_structures.h
var ufsVendorID = map[uint16]string{
	0x1CE: "Samsung",
	0x1AD: "SK Hynix",
	0x198: "Toshiba/Kioxia",
	0x12C: "Micron",
	0x02C: "Micron(ES)",
}

func partNo(id []byte, idLen uint32) string {
	u := usedID(id, idLen)
	if len(u) == 0 || !isASCIIPart(u) {
		return ""
	}
	return string(trimNul(u))
}

// pnmOf 取颗粒的 PNM：
//   - eMMC：CID 里第 4..9 字节是 6 字节产品名（PNM）
//   - UFS/uMCP：ID 本身就是 ASCII 料号，直接当 PNM
//
// 没有 ID 的通配记录返回空串。
func pnmOf(r Rec) string {
	u := usedID(r.ID, r.IDLen)
	if len(u) == 0 {
		return ""
	}
	if isASCIIPart(u) {
		return string(trimNul(u))
	}
	if len(u) >= 9 {
		return strings.Trim(strings.TrimSpace(printable(u[3:9])), ".")
	}
	return ""
}

// pnmText 给表格用，没有 PNM 时给个占位
func pnmText(r Rec) string {
	if p := pnmOf(r); p != "" {
		return p
	}
	return "—"
}

func cidDecode(id []byte, idLen uint32) string {
	u := usedID(id, idLen)
	if len(u) == 0 || isASCIIPart(u) {
		return ""
	}
	n := len(u)
	var b strings.Builder
	if n >= 1 {
		fmt.Fprintf(&b, "MID=0x%02X ", u[0])
	}
	if n >= 3 {
		fmt.Fprintf(&b, "CBX=0x%02X OID=0x%02X ", u[1], u[2])
	}
	if n >= 9 {
		fmt.Fprintf(&b, "PNM=\"%s\" ", printable(u[3:9]))
	}
	if n >= 10 {
		if spec := emmcSpec(u[9]); spec != "" {
			fmt.Fprintf(&b, "PRV=0x%02X(%s) ", u[9], spec)
		} else {
			fmt.Fprintf(&b, "PRV=0x%02X ", u[9])
		}
	}
	if n >= 15 {
		fmt.Fprintf(&b, "PSN=\"%s\" MDT=0x%02X", printable(u[10:14]), u[14])
	}
	return strings.TrimSpace(b.String())
}

func printable(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c <= 0x7E {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('.')
		}
	}
	return sb.String()
}

func hexID(id []byte, idLen uint32) string {
	u := usedID(id, idLen)
	if len(u) == 0 {
		return "(通配)"
	}
	return strings.ToUpper(strings.TrimSpace(fmt.Sprintf("% X", u)))
}

func wordNames(ver, n int) []string {
	// 10 个字的记录（v30+ 的 160 字节布局，以及部分 v25 的 144 字节布局）用 ACTIME0~7 那套名字；
	// 按字数判断比按版本判断可靠——同一个版本号在不同 SoC 上布局能不一样。
	if n == len(fields10) {
		return fields10
	}
	if n <= len(fields17) {
		return fields17[:n]
	}
	out := make([]string, n)
	copy(out, fields17)
	for i := len(fields17); i < n; i++ {
		out[i] = fmt.Sprintf("DRAMC_W%d", i)
	}
	return out
}

// ---------------------------------------------------------------- 排序

func sortRecords(recs []Rec, mode string) []Rec {
	out := append([]Rec(nil), recs...)
	switch mode {
	case "idx":
		// 保持原序
	case "cap":
		sort.SliceStable(out, func(i, j int) bool {
			return totalCap(out[i].Ranks) > totalCap(out[j].Ranks)
		})
	case "type":
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Type != out[j].Type {
				return out[i].Type > out[j].Type
			}
			return totalCap(out[i].Ranks) > totalCap(out[j].Ranks)
		})
	default: // speed
		sort.SliceStable(out, func(i, j int) bool {
			ri, rj := rateOf(out[i].Type), rateOf(out[j].Type)
			if ri != rj {
				return ri > rj
			}
			ci, cj := totalCap(out[i].Ranks), totalCap(out[j].Ranks)
			if ci != cj {
				return ci > cj
			}
			return out[i].Idx < out[j].Idx
		})
	}
	return out
}

// ---------------------------------------------------------------- 输出

func renderFile(fr *FileResult, opts Options) {
	line := strings.Repeat("=", 100)
	fmt.Fprintln(out, line)
	fmt.Fprintf(out, "文件: %s\n", fr.Path)
	fmt.Fprintf(out, "大小: %s (%d 字节)\n", fr.SizeHex, fr.Size)
	if fr.Platform.Basis != "" || fr.Platform.Name != "" {
		fmt.Fprintf(out, "平台: %s\n", platDisplay(fr.Platform))
	}
	fmt.Fprintf(out, "型号库: %s\n", DBStatus())
	if fr.Error != "" {
		fmt.Fprintf(out, "错误: %s\n", fr.Error)
		fmt.Fprintln(out, line)
		return
	}
	fmt.Fprintf(out, "找到 %d 张 MTK_BLOADER_INFO 表\n", len(fr.Tables))
	if fr.Skipped > 0 {
		fmt.Fprintf(out, "（另有 %d 处标记是残留数据误命中，已忽略）\n", fr.Skipped)
	}
	renderSummary(fr)
	for _, t := range fr.Tables {
		renderTable(t, opts)
	}
	fmt.Fprintln(out, line)
}

func countLine(m map[string]int) string {
	type kv struct {
		k string
		n int
	}
	var xs []kv
	for k, n := range m {
		xs = append(xs, kv{k, n})
	}
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].n != xs[j].n {
			return xs[i].n > xs[j].n
		}
		return xs[i].k < xs[j].k
	})
	var ss []string
	for _, x := range xs {
		ss = append(ss, fmt.Sprintf("%s ×%d", x.k, x.n))
	}
	return strings.Join(ss, "   ")
}

// renderSummary 在整个文件的表格之前给一份总览，方便一眼看清“这颗 preloader 挑哪些料”
func renderSummary(fr *FileResult) {
	if len(fr.Tables) == 0 {
		return
	}
	total, valid := 0, 0
	ddr, media, pack, vendor := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	ids := map[string]bool{}
	var minC, maxC uint64
	first := true
	for _, t := range fr.Tables {
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
			ids[fmt.Sprintf("%X/%X", r.ID, r.IDLen)] = true
			c := totalCap(r.Ranks)
			if c > 0 {
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
	if total == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "── 汇总 ──────────────────────────────────────────────────────────────")
	fmt.Fprintf(out, "  记录 %d 条（有效 %d）   唯一闪存 ID %d 组\n", total, valid, len(ids))
	fmt.Fprintf(out, "  DDR 代际 : %s\n", countLine(ddr))
	fmt.Fprintf(out, "  介质     : %s\n", countLine(media))
	fmt.Fprintf(out, "  封装     : %s\n", countLine(pack))
	fmt.Fprintf(out, "  厂商     : %s\n", countLine(vendor))
	if !first {
		if minC == maxC {
			fmt.Fprintf(out, "  容量     : %s\n", humanSize(maxC))
		} else {
			fmt.Fprintf(out, "  容量     : %s ~ %s\n", humanSize(minC), humanSize(maxC))
		}
	}
	fmt.Fprintln(out, "──────────────────────────────────────────────────────────────────────")
}

func renderTable(t *Table, opts Options) {
	fmt.Fprintln(out)
	if t.Truncated && t.CountWant > 0 {
		fmt.Fprintf(out, "┌─ 表 @ 0x%X  %s  条目 %d/%d（文件被截断）  记录 %d 字节\n",
			t.Offset, t.RawHead, t.Count, t.CountWant, t.RecSize)
	} else {
		fmt.Fprintf(out, "┌─ 表 @ 0x%X  %s  条目 %d  记录 %d 字节\n",
			t.Offset, t.RawHead, t.Count, t.RecSize)
	}
	if t.PreName != "" {
		fmt.Fprintf(out, "│  preloader : %s\n", t.PreName)
	}
	if t.MtkBin != "" || t.Check != [3]uint32{} {
		fmt.Fprintf(out, "│  标识      : %s   校验 %08X %08X %08X\n",
			t.MtkBin, t.Check[0], t.Check[1], t.Check[2])
	}
	if t.Note != "" {
		fmt.Fprintf(out, "│  ⚠ %s\n", t.Note)
	}
	if t.Err != "" {
		fmt.Fprintf(out, "│  !! %s\n", t.Err)
	}
	if t.Count == 0 {
		fmt.Fprintln(out, "│  （该表没有记录）")
		fmt.Fprintln(out, "└─")
		return
	}

	recs := sortRecords(t.Records, opts.Sort)
	validRecs := make([]Rec, 0, len(recs))
	var badIdx []string
	for _, r := range recs {
		if r.Valid {
			validRecs = append(validRecs, r)
		} else {
			badIdx = append(badIdx, "#"+strconv.Itoa(r.Idx))
		}
	}

	rows := [][]string{{
		"#", "存储", "封装", "DDR版本", "速率(标称)", "DRAM容量", "组成", "PNM", "丝印型号", "厂商", "零件号 / CID",
	}}
	hasSilk := false
	for _, r := range validRecs {
		silk := DBSilk(pnmOf(r), r.ID, r.IDLen, totalCap(r.Ranks))
		if silk != "-" {
			hasSilk = true
		}
		rows = append(rows, []string{
			strconv.Itoa(r.Idx),
			mediaKind(r, t.Suffix),
			packKind(r.Type),
			ddrKind(r.Type),
			strconv.Itoa(rateOf(r.Type)) + " MT/s",
			humanSize(totalCap(r.Ranks)),
			composition(r.Ranks),
			pnmText(r),
			silk,
			vendorOf(r.ID, r.IDLen),
			idCell(r),
		})
	}
	// 一条都没命中型号库时去掉“丝印型号”列，免得表格无谓变宽
	if !hasSilk && len(rows) > 0 {
		for i := range rows {
			rows[i] = append(rows[i][:8], rows[i][9:]...)
		}
	}
	if len(validRecs) == 0 {
		fmt.Fprintln(out, "│  （表里没有可用记录：全部记录字段不合理）")
		fmt.Fprintln(out, "└─")
		return
	}
	fmt.Fprint(out, "│\n")
	for _, ln := range strings.Split(strings.TrimRight(renderGrid(rows), "\n"), "\n") {
		fmt.Fprintln(out, "│  "+ln)
	}
	if len(badIdx) > 0 {
		fmt.Fprintf(out, "│  ⚠ 另有 %d 条记录字段不合理已隐藏：%s\n",
			len(badIdx), strings.Join(badIdx, " "))
	}
	fmt.Fprintln(out, "│")

	if opts.Verbose {
		fmt.Fprintln(out, "│  详细时序寄存器 / LPDDR MR：")
	} else {
		fmt.Fprintln(out, "│  时序寄存器 / LPDDR MR（行号 # 与上表一致；-v 可看全部寄存器）：")
	}
	for _, r := range validRecs {
		renderRecordDetail(r, t, opts.Verbose)
	}
	fmt.Fprintln(out, "└─")
}

func idCell(r Rec) string {
	if pn := partNo(r.ID, r.IDLen); pn != "" {
		return pn
	}
	return hexID(r.ID, r.IDLen)
}

// mrText 把 MR 表拼成 "MR1=0x83 MR2=0x0A ..."；
// 表里出现同一个 MR 号两次时（部分老 preloader 确实这样写）标注出来，避免误以为解析错。
func mrText(mrs []MRPair, wide bool) string {
	seen := map[uint16]int{}
	var ss []string
	for _, m := range mrs {
		seen[m.Reg]++
		v := m.Val & 0xFF
		s := fmt.Sprintf("MR%d=0x%02X", m.Reg, v)
		if wide {
			s = fmt.Sprintf("MR%d=0x%04X", m.Reg, m.Val)
		}
		if seen[m.Reg] > 1 {
			s = fmt.Sprintf("MR%d=0x%02X(表内重复)", m.Reg, v)
		}
		ss = append(ss, s)
	}
	return strings.Join(ss, " ")
}

// keyWords 取紧凑模式下要显示的关键时序寄存器
func keyWords(names []string, w []uint32) string {
	keys := []string{
		"EMI_CONA", "EMI_CONH", "DRAMC_ACTIM", "DRAMC_CONF1",
		"DRAMC_ACTIM1", "DRAMC_ACTIM05T",
	}
	var ss []string
	for _, k := range keys {
		for i, n := range names {
			if n == k && i < len(w) {
				ss = append(ss, fmt.Sprintf("%s=0x%08X", shortName(n), w[i]))
			}
		}
	}
	return strings.Join(ss, " ")
}

func renderRecordDetail(r Rec, t *Table, verbose bool) {
	names := wordNames(t.Version, len(r.Words))
	regs := keyWords(names, r.Words)

	if !verbose {
		line := fmt.Sprintf("#%-3d", r.Idx)
		if regs != "" {
			line += regs
		}
		if len(r.MR) > 0 {
			line += "  | " + mrText(r.MR, false)
		}
		if !r.Valid {
			line += "  ⚠ " + r.Note
		}
		fmt.Fprintf(out, "│  %s\n", line)
		return
	}

	fmt.Fprintf(out, "│  [#%d] type=0x%03X  %s / %s / %s\n",
		r.Idx, r.Type, ddrKind(r.Type), packKind(r.Type), mediaKind(r, t.Suffix))
	if !r.Valid {
		fmt.Fprintf(out, "│        ⚠ %s\n", r.Note)
	}
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
	fmt.Fprintln(out, kv("厂商", vendorOf(r.ID, r.IDLen)))
	if pn := pnmOf(r); pn != "" {
		fmt.Fprintln(out, kv("PNM", pn))
	}
	for _, k := range []string{"丝印型号", "eMMC版本"} {
		if v := dbv[k]; v != "" {
			fmt.Fprintln(out, kv(k, v))
		}
	}
	if c := cidDecode(r.ID, r.IDLen); c != "" {
		fmt.Fprintln(out, kv("CID 解码", c))
	}
	fmt.Fprintln(out, kv("ID", fmt.Sprintf("%d 字节  %s", r.IDLen, hexID(r.ID, r.IDLen))))
	for _, k := range []string{"实测速度", "推荐指数", "常见年份与设备", "备注"} {
		if v := dbv[k]; v != "" {
			fmt.Fprintln(out, kv(k, v))
		}
	}
	if pn := partNo(r.ID, r.IDLen); pn != "" {
		fmt.Fprintln(out, kv("料号", pn))
	}
	fmt.Fprintln(out, kv("DRAM", fmt.Sprintf("%s  标称 %s  容量 %s  组成 %s",
		ddrKind(r.Type), rateText(r.Type), humanSize(totalCap(r.Ranks)), composition(r.Ranks))))
	fmt.Fprintln(out, kv("rank 原始", "["+joinHex(r.Ranks)+"]"))
	for _, l := range dbRest {
		fmt.Fprintln(out, kv(l[0], l[1]))
	}
	if v := dbv["匹配依据"]; v != "" {
		fmt.Fprintln(out, kv("匹配依据", v))
	}
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
		fmt.Fprintln(out, pad("│        "+n, 33)+"= "+fmt.Sprintf("0x%08X", w))
	}
	if zeros > 0 {
		fmt.Fprintf(out, "│        （其余 %d 个寄存器为 0）\n", zeros)
	}
	if len(r.MR) > 0 {
		fmt.Fprintln(out, kv("LPDDR MR", mrText(r.MR, true)))
	}
}

func kv(label, val string) string {
	return "│        " + pad(label, 15) + ": " + val
}

func shortName(n string) string {
	if strings.HasPrefix(n, "DRAMC_") {
		return n[len("DRAMC_"):]
	}
	return n
}

func compShort(ranks []uint64) string {
	c := composition(ranks)
	return strings.ReplaceAll(c, "运行时自检", "自检")
}

func joinHex(v []uint64) string {
	ss := make([]string, 0, len(v))
	for _, x := range v {
		ss = append(ss, fmt.Sprintf("0x%X", x))
	}
	return strings.Join(ss, ", ")
}

// renderGrid 生成对齐的表格（中文按 2 列宽计算）
func renderGrid(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := len(rows[0])
	w := make([]int, cols)
	for _, r := range rows {
		for i := 0; i < cols && i < len(r); i++ {
			if d := dwidth(r[i]); d > w[i] {
				w[i] = d
			}
		}
	}
	var b strings.Builder
	for ri, r := range rows {
		for i := 0; i < cols && i < len(r); i++ {
			b.WriteString(pad(r[i], w[i]))
			if i < cols-1 {
				b.WriteString("  ")
			}
		}
		b.WriteString("\n")
		if ri == 0 {
			for i := 0; i < cols; i++ {
				b.WriteString(strings.Repeat("-", w[i]))
				if i < cols-1 {
					b.WriteString("  ")
				}
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func dwidth(s string) int {
	w := 0
	for _, r := range s {
		if isWide(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func pad(s string, width int) string {
	d := dwidth(s)
	if d >= width {
		return s
	}
	return s + strings.Repeat(" ", width-d)
}

func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0x303E,
		r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x4E00 && r <= 0x9FFF,
		r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD:
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- 报告文件

func exeDir() string {
	if p, err := os.Executable(); err == nil {
		return filepath.Dir(p)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func reportPath(path string, opts Options) string {
	dir := opts.OutDir
	if dir == "" {
		dir = exeDir()
	}
	if strings.EqualFold(dir, "same") {
		dir = filepath.Dir(path)
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return filepath.Join(dir, base+".emi.txt")
}

func writeReport(path string, fr *FileResult, opts Options) {
	dst := reportPath(path, opts)

	var b strings.Builder
	fmt.Fprintf(&b, "MTK preloader EMI 配置解析报告\n")
	fmt.Fprintf(&b, "生成时间: %s\n", nowStr())

	// 报告文件里始终输出完整详情，不受 -v 影响
	ropts := opts
	ropts.Verbose = true

	saved := out
	out = &b
	renderFile(fr, ropts)
	out = saved

	writeUTF8File(dst, b.String())
	if !opts.QuietReport {
		fmt.Fprintf(out, "\n报告已写入: %s\n", dst)
	}

	if opts.CSV {
		writeCSV(strings.TrimSuffix(dst, ".emi.txt")+".emi.csv", fr)
	}
}

func writeCSV(path string, fr *FileResult) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString("\uFEFF")
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"表偏移", "preloader", "序号", "type", "存储", "封装", "DDR版本",
		"速率MT/s(标称)", "DRAM容量(字节)", "组成", "PNM", "厂商", "零件号", "IDHEX", "rank0", "rank1", "rank2", "rank3"})
	for _, t := range fr.Tables {
		for _, r := range t.Records {
			_ = w.Write([]string{
				fmt.Sprintf("0x%X", t.Offset), t.PreName, strconv.Itoa(r.Idx),
				fmt.Sprintf("0x%03X", r.Type), mediaKind(r, t.Suffix), packKind(r.Type),
				ddrKind(r.Type), strconv.Itoa(rateOf(r.Type)),
				strconv.FormatUint(totalCap(r.Ranks), 10), composition(r.Ranks),
				pnmOf(r), vendorOf(r.ID, r.IDLen), partNo(r.ID, r.IDLen), hexID(r.ID, r.IDLen),
				strconv.FormatUint(r.Ranks[0], 10), strconv.FormatUint(r.Ranks[1], 10),
				strconv.FormatUint(r.Ranks[2], 10), strconv.FormatUint(r.Ranks[3], 10),
			})
		}
	}
	fmt.Fprintf(out, "CSV 已写入: %s\n", path)
}

func writeUTF8File(path, s string) {
	_ = os.WriteFile(path, append([]byte{0xEF, 0xBB, 0xBF}, []byte(s)...), 0644)
}

// ---------------------------------------------------------------- JSON 输出
// 给外部脚本用的结构化输出：字段是解码后的语义值，不是原始结构体

type jsonKV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type jsonRec struct {
	Idx         int      `json:"idx"`
	Type        string   `json:"type"`
	Media       string   `json:"media"`
	Package     string   `json:"package"`
	DDR         string   `json:"ddr"`
	SpeedMTS    int      `json:"speed_mts"`
	Capacity    string   `json:"capacity"`
	CapacityB   uint64   `json:"capacity_bytes"`
	Composition string   `json:"composition"`
	Ranks       []string `json:"ranks"`
	PNM         string   `json:"pnm,omitempty"`
	Silk        string   `json:"silk,omitempty"`
	EmmcSpec    string   `json:"emmc_spec,omitempty"`
	SpeedMeas   string   `json:"speed_measured,omitempty"`
	SpeedCond   string   `json:"speed_cond,omitempty"`
	Vendor      string   `json:"vendor"`
	PartNo      string   `json:"part_no,omitempty"`
	IDHex       string   `json:"id_hex"`
	CID         string   `json:"cid,omitempty"`
	Registers   []jsonKV `json:"registers"`
	MR          []jsonKV `json:"mr,omitempty"`
	Valid       bool     `json:"valid"`
	Note        string   `json:"note,omitempty"`
}

type jsonTable struct {
	Offset    string    `json:"offset"`
	Head      string    `json:"head"`
	Version   int       `json:"version"`
	Suffix    string    `json:"suffix,omitempty"`
	Preloader string    `json:"preloader"`
	Count     int       `json:"count"`
	CountHead int       `json:"count_in_header,omitempty"`
	RecSize   int       `json:"rec_size"`
	Truncated bool      `json:"truncated,omitempty"`
	Note      string    `json:"note,omitempty"`
	Error     string    `json:"error,omitempty"`
	Records   []jsonRec `json:"records"`
}

type jsonFile struct {
	File     string       `json:"file"`
	Size     int64        `json:"size"`
	SizeHx   string       `json:"size_hex"`
	Platform PlatformInfo `json:"platform"`
	Error    string       `json:"error,omitempty"`
	Tables   []jsonTable  `json:"tables"`
}

func buildJSON(all []*FileResult, opts Options) []jsonFile {
	res := make([]jsonFile, 0, len(all))
	for _, fr := range all {
		jf := jsonFile{File: fr.Path, Size: fr.Size, SizeHx: fr.SizeHex, Platform: fr.Platform,
			Error: fr.Error, Tables: []jsonTable{}}
		for _, t := range fr.Tables {
			jt := jsonTable{
				Offset:    fmt.Sprintf("0x%X", t.Offset),
				Head:      t.RawHead,
				Version:   t.Version,
				Suffix:    t.Suffix,
				Preloader: t.PreName,
				Count:     t.Count,
				CountHead: t.CountWant,
				RecSize:   t.RecSize,
				Truncated: t.Truncated,
				Note:      t.Note,
				Error:     t.Err,
				Records:   []jsonRec{},
			}
			for _, r := range sortRecords(t.Records, opts.Sort) {
				jr := jsonRec{
					Idx:         r.Idx,
					Type:        fmt.Sprintf("0x%03X", r.Type),
					Media:       mediaKind(r, t.Suffix),
					Package:     packKind(r.Type),
					DDR:         ddrKind(r.Type),
					SpeedMTS:    rateOf(r.Type),
					Capacity:    humanSize(totalCap(r.Ranks)),
					CapacityB:   totalCap(r.Ranks),
					Composition: composition(r.Ranks),
					Ranks:       make([]string, 0, 4),
					PNM:         pnmOf(r),
					Vendor:      vendorOf(r.ID, r.IDLen),
					PartNo:      partNo(r.ID, r.IDLen),
					IDHex:       hexID(r.ID, r.IDLen),
					CID:         cidDecode(r.ID, r.IDLen),
					Valid:       r.Valid,
					Note:        r.Note,
				}
				for _, x := range r.Ranks {
					jr.Ranks = append(jr.Ranks, fmt.Sprintf("0x%X", x))
				}
				jr.Silk, jr.EmmcSpec, jr.SpeedMeas, jr.SpeedCond =
					DBFlat(jr.PNM, jr.IDHex, jr.CapacityB)
				names := wordNames(t.Version, len(r.Words))
				for i, w := range r.Words {
					n := fmt.Sprintf("W%d", i)
					if i < len(names) {
						n = names[i]
					}
					jr.Registers = append(jr.Registers, jsonKV{n, fmt.Sprintf("0x%08X", w)})
				}
				for _, m := range r.MR {
					jr.MR = append(jr.MR, jsonKV{fmt.Sprintf("MR%d", m.Reg), fmt.Sprintf("0x%04X", m.Val)})
				}
				jt.Records = append(jt.Records, jr)
			}
			jf.Tables = append(jf.Tables, jt)
		}
		res = append(res, jf)
	}
	return res
}

func nowStr() string {
	return timeNow()
}
func pause(noPause bool) {
	if noPause {
		return
	}
	fmt.Fprint(out, "\n按回车键退出...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// 确保 utf8 被引用（防止未来裁剪 import）
var _ = utf8.RuneLen
