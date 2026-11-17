package main

// PNM 数据库：把《常见BGA221-eMCP汇总》里“系统内 PNM 型号 → 丝印型号 / eMMC 版本 / 实测速度”
// 的对照表内置到 exe 里。首次运行时解压一份明文 TSV 到 exe 同目录，之后都读这个文件
// （改完保存、重开软件即可生效）。内置副本用 SHA-256 做完整性校验，界面上会显示“内置/已修改”。

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

//go:embed pnmdB.tsv.gz
var pnmDBGz []byte

const pnmDBFileName = "mtk_emi_pnm_db.tsv"

type pnmRow struct {
	PNM       string
	DramGB    float64
	EmmcGB    float64
	Vendor    string
	VendorCN  string
	Silk      string
	Years     string
	CID       string
	Speed     string
	IO        string
	Recommend string
	Status    string
	Devices   string
	Note      string
}

type pnmDB struct {
	rows    []pnmRow
	byPNM   map[string][]int
	byCID   map[string][]int
	version string
	path    string
	builtin bool // 当前用的是内置副本（文件不存在/读不出来）
	edited  bool // 文件内容与内置副本不一致（被改过）
	sum     string
	err     string
	loaded  bool
}

var gdb pnmDB
var dbOnce sync.Once

// DB 返回（并首次加载）内置型号库
func DB() *pnmDB {
	dbOnce.Do(loadDB)
	return &gdb
}

// ReloadDB 重新读磁盘上的数据库文件（“修改内置数据库”“重新载入”用）
func ReloadDB() {
	loadDB()
}

func gunzip(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty")
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(zr); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func loadDB() {
	gdb.path = filepath.Join(exeDir(), pnmDBFileName)
	gdb.rows = nil
	gdb.byPNM = map[string][]int{}
	gdb.byCID = map[string][]int{}
	gdb.version = ""
	gdb.builtin = false
	gdb.edited = false
	gdb.sum = ""
	gdb.err = ""

	builtin, err := gunzip(pnmDBGz)
	if err != nil {
		gdb.err = "内置数据库损坏：" + err.Error()
		gdb.loaded = true
		return
	}
	bs := sha256.Sum256(builtin)

	// 首次运行（或用户删了）：把内置库解压到 exe 同目录
	if _, statErr := os.Stat(gdb.path); statErr != nil {
		if werr := os.WriteFile(gdb.path, builtin, 0o644); werr == nil {
			gdb.sum = hex.EncodeToString(bs[:8])
		}
	}

	raw, rerr := os.ReadFile(gdb.path)
	switch {
	case rerr != nil || len(raw) == 0:
		raw = builtin
		gdb.builtin = true
		gdb.sum = hex.EncodeToString(bs[:8])
	case sha256.Sum256(raw) == bs:
		gdb.sum = hex.EncodeToString(bs[:8])
	default:
		gdb.edited = true
		s := sha256.Sum256(raw)
		gdb.sum = hex.EncodeToString(s[:8])
	}

	if err := gdb.parse(raw); err != nil {
		gdb.err = err.Error()
	}
	gdb.loaded = true
}

func (d *pnmDB) parse(raw []byte) error {
	d.rows = d.rows[:0]
	d.byPNM = map[string][]int{}
	d.byCID = map[string][]int{}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "#"))
			if strings.HasPrefix(strings.ToUpper(v), "VERSION") {
				d.version = strings.TrimSpace(v[len("VERSION"):])
			}
			continue
		}
		f := strings.Split(line, "\t")
		for len(f) < 14 {
			f = append(f, "")
		}
		row := pnmRow{
			PNM:       strings.TrimSpace(f[0]),
			Vendor:    strings.TrimSpace(f[3]),
			VendorCN:  strings.TrimSpace(f[4]),
			Silk:      strings.TrimSpace(f[5]),
			Years:     strings.TrimSpace(f[6]),
			CID:       strings.ToLower(strings.TrimSpace(f[7])),
			Speed:     strings.TrimSpace(f[8]),
			IO:        strings.TrimSpace(f[9]),
			Recommend: strings.TrimSpace(f[10]),
			Status:    strings.TrimSpace(f[11]),
			Devices:   strings.TrimSpace(f[12]),
			Note:      strings.TrimSpace(f[13]),
		}
		row.DramGB, _ = strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
		row.EmmcGB, _ = strconv.ParseFloat(strings.TrimSpace(f[2]), 64)
		if row.PNM == "" || row.PNM == "/" {
			continue
		}
		i := len(d.rows)
		d.rows = append(d.rows, row)
		k := strings.ToUpper(row.PNM)
		d.byPNM[k] = append(d.byPNM[k], i)
		if len(row.CID) >= 18 {
			ck := strings.ToUpper(row.CID[:18])
			d.byCID[ck] = append(d.byCID[ck], i)
		}
	}
	if len(d.rows) == 0 {
		return fmt.Errorf("数据库里没有有效行")
	}
	return nil
}

// ---------------------------------------------------------------- 查询

type pnmHit struct {
	Rows []pnmRow
	How  string // 命中方式，给人看的
}

func (h pnmHit) empty() bool { return len(h.Rows) == 0 }

// DBLookup 按 PNM + DRAM 容量（+ CID 前缀）查内置库。
// 海力士同一个 PNM 会对应多种容量（2+32 / 3+32 / 4+32），只能靠 EMI 表里的 DRAM 容量区分。
func DBLookup(pnm, idHex string, capBytes uint64) pnmHit {
	d := DB()
	key := strings.ToUpper(strings.TrimSpace(pnm))
	idKey := strings.ToUpper(strings.TrimSpace(idHex))
	if len(idKey) >= 18 {
		idKey = idKey[:18]
	} else {
		idKey = ""
	}

	idxs := d.byPNM[key]
	if len(idxs) == 0 {
		if idKey != "" {
			if c := d.byCID[idKey]; len(c) > 0 {
				return pnmHit{Rows: d.pick(c), How: "按 CID 匹配（PNM 对不上）"}
			}
		}
		return pnmHit{}
	}
	if len(idxs) == 1 {
		e := d.rows[idxs[0]]
		capGB := float64(capBytes) / (1 << 30)
		if capGB > 0 && e.DramGB > 0 && math.Abs(e.DramGB-capGB) > 0.05 {
			return pnmHit{Rows: d.pick(idxs),
				How: fmt.Sprintf("库里只有一颗（%g+%g），但记录读到 %gGB DRAM，可能不是同一颗",
					e.DramGB, e.EmmcGB, capGB)}
		}
		return pnmHit{Rows: d.pick(idxs), How: "唯一型号"}
	}

	// 同 PNM 多行：先用 EMI 里的 DRAM 容量分（这是设备上真实读到的，最可信），
	// 再用 CID 分（同一个 PNM 可能有 '!'/无 '!' 之类的细微差别）。
	capGB := float64(capBytes) / (1 << 30)
	if capGB > 0 {
		var c []int
		for _, i := range idxs {
			if d.rows[i].DramGB > 0 && math.Abs(d.rows[i].DramGB-capGB) < 0.05 {
				c = append(c, i)
			}
		}
		if len(c) == 1 {
			return pnmHit{Rows: d.pick(c), How: "按 DRAM 容量匹配（同 PNM 多容量）"}
		}
		if len(c) > 1 {
			idxs = c
		}
	}
	if len(idxs) > 1 && idKey != "" {
		var c []int
		for _, i := range idxs {
			if len(d.rows[i].CID) >= 18 && strings.EqualFold(d.rows[i].CID[:18], idKey) {
				c = append(c, i)
			}
		}
		if len(c) == 1 {
			return pnmHit{Rows: d.pick(c), How: "按 CID 匹配（同 PNM 同容量）"}
		}
		if len(c) > 1 {
			idxs = c
		}
	}
	if len(idxs) == 1 {
		return pnmHit{Rows: d.pick(idxs), How: "同 PNM 仅剩一个候选"}
	}
	return pnmHit{Rows: d.pick(idxs),
		How: fmt.Sprintf("同 PNM 有 %d 个候选，容量/CID 都对不上，全部列出", len(idxs))}
}

func (d *pnmDB) pick(idx []int) []pnmRow {
	out := make([]pnmRow, 0, len(idx))
	for _, i := range idx {
		out = append(out, d.rows[i])
	}
	return out
}

// DBStatus 给标题栏/关于框用
func DBStatus() string {
	d := DB()
	ver := d.version
	if ver == "" {
		ver = "未知版本"
	}
	s := fmt.Sprintf("数据库 %s（%d 条", ver, len(d.rows))
	switch {
	case d.err != "":
		s += "，加载出错）"
	case d.builtin:
		s += "，内置副本）"
	case d.edited:
		s += "，本地已修改）"
	default:
		s += "，校验一致）"
	}
	return s
}

// ---------------------------------------------------------------- 展示

var (
	reSpecVer = regexp.MustCompile(`(\d+(?:\.\d+)?)`)
	reRW      = regexp.MustCompile(`(?i)r\s*(\d+)\s*[/／]?\s*[rw]\s*(\d+)`)
	reW       = regexp.MustCompile(`(?i)w\s*(\d+)`)
	reLife    = regexp.MustCompile(`(?i)0x([0-9a-f]{2})`)
	reParen   = regexp.MustCompile(`[（(]([^）)]*)[）)]`)
)

// lifeBand：eMMC EXT_CSD 的 DEVICE_LIFE_TIME_EST 编码 → 寿命已用区间（0x01 = 0~10%，0x02 = 10~20% ……）
func lifeBand(n int) (string, bool) {
	if n <= 0 {
		return "", false
	}
	if n <= 0x0A {
		return fmt.Sprintf("%d~%d%%", (n-1)*10, n*10), true
	}
	return "已超出最大寿命", true
}

// specText：'5.1MLC/1866' → 'eMMC 5.1 · MLC · DDR 1866 MHz'
func specText(s string) string {
	if s == "" {
		return ""
	}
	var parts []string
	ver := ""
	if m := reSpecVer.FindStringSubmatch(s); m != nil {
		ver = m[1]
	}
	if ver != "" {
		parts = append(parts, "eMMC "+ver)
	}
	up := strings.ToUpper(s)
	switch {
	case strings.Contains(up, "MLC"):
		parts = append(parts, "MLC")
	case strings.Contains(up, "TLC"):
		parts = append(parts, "TLC")
	}
	if i := strings.Index(s, "/"); i >= 0 {
		if ddr := strings.TrimSpace(s[i+1:]); ddr != "" {
			if _, err := strconv.Atoi(ddr); err == nil {
				parts = append(parts, "DDR "+ddr+" MHz")
			} else {
				parts = append(parts, ddr)
			}
		}
	}
	return strings.Join(parts, " · ")
}

// ioText：'HS400 r290w125（0x01）' → 速度 + 括号里的测试条件
//
//	speed = "读 290 写 125 MB/s"
//	cond  = "测试平台：HS400，寿命 0~10% · 0x01"；括号里没东西就只有 "测试平台：HS200"
//
// 寿命数据就是表格里括号中的那个 EXT_CSD 编码，没写就空着，不编造“空盘/半盘”。
// 表格里没标 HS400 的按 HS200 算。
func ioText(io string) (speed, cond string) {
	src := strings.TrimSpace(io)
	if src == "" {
		return "", ""
	}
	up := strings.ToUpper(src)
	// 表格里没写 HS400 的一律按 HS200 标（老 eMMC 4.5 时代的通用速率档）
	mode := "HS200"
	if strings.Contains(up, "HS400") {
		mode = "HS400"
	}

	switch {
	case reRW.MatchString(src):
		m := reRW.FindStringSubmatch(src)
		speed = "读 " + m[1] + " 写 " + m[2] + " MB/s"
	case reW.MatchString(src):
		m := reW.FindStringSubmatch(src)
		speed = "写 " + m[1] + " MB/s"
	default:
		speed = strings.TrimSpace(reParen.ReplaceAllString(src, ""))
		speed = strings.TrimSpace(strings.NewReplacer("HS400", "", "hs400", "", "HS200", "", "hs200", "").Replace(speed))
	}

	parts := []string{"测试平台：" + mode}
	// 只认括号里的内容当测试条件（寿命就是括号里那个 0xNN）
	for _, g := range reParen.FindAllStringSubmatch(src, -1) {
		body := strings.TrimSpace(g[1])
		if body == "" {
			continue
		}
		if m := reLife.FindStringSubmatch(body); m != nil {
			if n, err := strconv.ParseInt(m[1], 16, 32); err == nil {
				if band, ok := lifeBand(int(n)); ok {
					parts = append(parts, fmt.Sprintf("寿命 %s · 0x%02X", band, n))
					// 括号里除了编码还写了别的（比如“设备端”），保留
					if extra := strings.Trim(strings.TrimSpace(reLife.ReplaceAllString(body, "")), "，, +、"); extra != "" {
						parts = append(parts, extra)
					}
					continue
				}
			}
		}
		// 括号里不是编码：原样保留（清零片 / 空盘 / 设备端 …）
		if t := strings.Trim(strings.TrimSpace(body), "，, +、"); t != "" {
			parts = append(parts, t)
		}
	}
	// 括号外面单独写的测试状态（“HS400 r300w210 清零”这种）也带上
	outside := reParen.ReplaceAllString(src, "")
	joined := strings.Join(parts, "")
	for _, kw := range []string{"清零", "空盘", "半盘", "设备端"} {
		if strings.Contains(outside, kw) && !strings.Contains(joined, kw) {
			parts = append(parts, kw)
			joined += kw
		}
	}
	return speed, strings.Join(parts, "，")
}

// ioLine：详情里那一行
func ioLine(io string) string {
	sp, cond := ioText(io)
	if sp == "" {
		return ""
	}
	if cond == "" {
		return sp
	}
	return sp + "（" + cond + "）"
}

// pnmSummary 一行式：'H9TQ27ADFTMC · eMMC 5.1 · DDR 1866'
func pnmSummary(e pnmRow) string {
	var parts []string
	if e.Silk != "" {
		parts = append(parts, e.Silk)
	}
	if sp := specText(e.Speed); sp != "" {
		parts = append(parts, sp)
	}
	if len(parts) == 0 {
		return "（库里只有 CID，没有丝印资料）"
	}
	return strings.Join(parts, " · ")
}

// DBSilk 返回该记录在型号库里的原始丝印型号；没命中或库里没记返回 "-"
func DBSilk(pnm string, id []byte, idLen uint32, capBytes uint64) string {
	hit := DBLookup(pnm, hexID(id, idLen), capBytes)
	if hit.empty() {
		return "-"
	}
	var ss []string
	for _, e := range hit.Rows {
		if e.Silk != "" {
			ss = append(ss, e.Silk)
		}
	}
	if len(ss) == 0 {
		return "-"
	}
	return strings.Join(ss, " / ")
}

// DBFlat 把库命中的信息拍平成四个字段，给 JSON 用（cond 是括号里的测试条件：接口速率 + 寿命基准）
func DBFlat(pnm, idHex string, capBytes uint64) (silk, spec, speed, cond string) {
	hit := DBLookup(pnm, idHex, capBytes)
	if hit.empty() || len(hit.Rows) != 1 {
		return "", "", "", ""
	}
	e := hit.Rows[0]
	sp, lf := ioText(e.IO)
	return e.Silk, specText(e.Speed), sp, lf
}

// DBDetailLines 给详情面板/报告用的一组行（标签 + 值），没有命中就返回 nil
func DBDetailLines(pnm string, id []byte, idLen int, capBytes uint64) [][2]string {
	var out [][2]string
	hit := DBLookup(pnm, hexID(id, uint32(idLen)), capBytes)
	if hit.empty() {
		return out
	}
	if len(hit.Rows) == 1 {
		e := hit.Rows[0]
		if e.Silk != "" {
			out = append(out, [2]string{"丝印型号", e.Silk})
		} else {
			out = append(out, [2]string{"丝印型号", "（库里没记）"})
		}
		if sp := specText(e.Speed); sp != "" {
			out = append(out, [2]string{"eMMC版本", sp})
		}
		if sp, _ := ioText(e.IO); sp != "" {
			out = append(out, [2]string{"实测速度", ioLine(e.IO)})
		} else {
			out = append(out, [2]string{"实测速度", "库里没记（上面的是规格标称，不是实测）"})
		}
		if e.Recommend != "" && e.Recommend != "/" {
			out = append(out, [2]string{"推荐指数", e.Recommend})
		}
		var dev []string
		if e.Years != "" && e.Years != "/" {
			dev = append(dev, e.Years)
		}
		if e.Devices != "" && e.Devices != "/" {
			dev = append(dev, e.Devices)
		}
		if e.Status != "" && e.Status != "/" {
			dev = append(dev, e.Status)
		}
		if len(dev) > 0 {
			out = append(out, [2]string{"常见年份与设备", strings.Join(dev, " · ")})
		}
		if e.Note != "" {
			out = append(out, [2]string{"备注", e.Note})
		}
		if hit.How != "唯一型号" && hit.How != "按 CID 匹配" {
			out = append(out, [2]string{"匹配依据", hit.How})
		}
		return out
	}
	// 多个候选：逐条列出来
	for i, e := range hit.Rows {
		label := "候选 " + strconv.Itoa(i+1)
		desc := fmt.Sprintf("%gGB", e.DramGB)
		if e.EmmcGB > 0 {
			desc += fmt.Sprintf("+%gGB", e.EmmcGB)
		}
		desc += "  " + pnmSummary(e)
		if sp, _ := ioText(e.IO); sp != "" {
			desc += "，实测 " + ioLine(e.IO)
		}
		out = append(out, [2]string{label, desc})
	}
	out = append(out, [2]string{"匹配依据", hit.How})
	return out
}
