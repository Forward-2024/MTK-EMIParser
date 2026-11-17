package main

// 这几个测试只是拿来核对展示文案，正常构建不受影响：
//   go test -run TestIO -v

import (
	"strings"
	"testing"
)

func TestLifeBand(t *testing.T) {
	cases := []struct {
		n    int
		want string
		ok   bool
	}{
		{0x00, "", false},
		{0x01, "0~10%", true},
		{0x02, "10~20%", true},
		{0x03, "20~30%", true},
		{0x0A, "90~100%", true},
		{0x0B, "已超出最大寿命", true},
	}
	for _, c := range cases {
		got, ok := lifeBand(c.n)
		if got != c.want || ok != c.ok {
			t.Errorf("lifeBand(0x%02X) = %q,%v want %q,%v", c.n, got, ok, c.want, c.ok)
		}
	}
}

func TestIOFormat(t *testing.T) {
	cases := []string{
		"HS400 r260w80（0x04）",
		"HS400 r290w150（0x02）",
		"r155/w45 (0x0b)",
		"r145/w45（0x02）设备端",
		"r90/r45 （0x01）",
		"r65w40（0x05）",
		"w120 (0x01)",
		"r150/w20 (清零片)",
		"r135/w20 (emmc4.5无寿命)",
		"HS400 r300w100（F2FS）",
		"HS400 r300w210 清零",
		"HS400 r280/w75 (0x01+)",
		"r150 w70 （0x04设备端）",
		"r160/w30",
		"",
	}
	for _, c := range cases {
		t.Logf("%-28q → %q", c, ioLine(c))
	}
	if got := ioLine("HS400 r260w80（0x04）"); got != "读 260 写 80 MB/s（测试平台：HS400，寿命 30~40% · 0x04）" {
		t.Errorf("HS400 0x04 文案不对：%q", got)
	}
	if got := ioLine("r160/w45 (0x01)"); !strings.Contains(got, "测试平台：HS200") {
		t.Errorf("没标 HS400 的应该按 HS200：%q", got)
	}
	if got := ioLine("r90/r45 （0x01）"); !strings.Contains(got, "读 90 写 45") {
		t.Errorf("r90/r45 应识别为读写：%q", got)
	}
}

func TestDBAllIO(t *testing.T) {
	d := DB()
	n := 0
	for _, r := range d.rows {
		if r.IO == "" {
			continue
		}
		n++
		t.Logf("%-8s %-30q → %q", r.PNM, r.IO, ioLine(r.IO))
	}
	t.Logf("共 %d 条有实测速度记录（库 %d 条）", n, len(d.rows))
}
