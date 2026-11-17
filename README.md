# MTKEmiParser

此项目由AI使用GO语言进行制作，软件内部数据库来源于本人整理，转载请注明出处
此为初版发布，部分闪存丝印与测速信息可能不准确或缺失
This project was created using the Go language by AI. The internal database of the software comes from my own compilation, please credit the source if reprinting. 
This is the first version release, so some flash silkscreen and speed test information may be inaccurate or missing.




MTK preloader / boot1 里那张 `MTK_BLOADER_INFO_vXX` EMI 表的解析器。
单文件 exe，无任何外部依赖，只读文件，不改任何东西。

**双击就是窗口界面**，把 `preloader_xxx.bin`、`boot1.bin`、`PRELOADER.img`（甚至整个设备备份目录）拖进窗口即可。

---

## 一、能看出什么

| 项目 | 来源 | 说明 |
|---|---|---|
| 文件内的 preloader 名 | 表头之后那段 `preloader_xxx.bin` 字符串 | 文件被改过名也能认出原始 preloader（窗口左上第 3 行） |
| 平台（SoC） | 文件里的 `MTxxxx` 串 / 文件名里的项目代号 | **是猜的**，窗口左上第 4 行，永远带"不一定准"，见下面「平台识别」 |
| 支持的闪存型号 | 记录里的 `m_emmc_id[16]` / `m_ufs_id[16]` | eMMC 是 9 字节 CID（厂商、PNM、PSN、MDT），uMCP/UFS 直接就是料号字符串 |
| 丝印型号 / eMMC 版本 / 实测速度 | 内置的 **PNM 型号库**（不是 preloader 里的数据） | 用 CID 里的 PNM + EMI 里的 DRAM 容量去查表，给出原始丝印料号、eMMC 规范版本、读写速度和**测速时的寿命基准**，见第九节 |
| 是否挑型号 | `m_id_length` | `0` = 不绑定具体颗粒（就是"不挑型号、固定 DRAM 容量"那种通配记录） |
| 闪存介质 | ID 的形态 | eMMC 的 ID 是二进制 CID，UFS/uMCP 的 ID 是 ASCII 料号 —— 两者格式完全不同，按格式判定最准 |
| 封装形式 | `m_type` 高字节 | 0 分离式、1 NAND-MCP、2 eMCP、3 uMCP |
| DDR 版本 | `m_type` 低字节 | 1 DDR1 … 5 LPDDR4、6 LPDDR4X、7 LPDDR5、8 LPDDR5X |
| DRAM 容量 | `m_dram_rank_size[4]` | 各 rank 相加 |
| 组成 | 同上 | `1GB×2`、`512MB×2`、`3GB×1` 这种（窗口里显示为 `1GB*2`，见第八节第 5 条）；全 0 时显示"运行时自检"（容量由 DRAM 自己报） |
| 时序寄存器 | `dramc0[]` / `ACTimeEMI[]` | EMI_CONA、DRAMC_ACTIM、DRAMC_CONF1 等，寄存器名按版本给 |
| LPDDR MR | 记录尾部 24 字节 | MR1/MR2/MR3/MR6/MR10/MR63 的写入值 |
| 排序 | — | 窗口默认按 preloader 里的原始顺序（表里第几条就是第几条）；勾选「按速率排序（代际标称，非实测）」后按速率降序、同速率容量降序。命令行 `-sort speed/cap/idx/type` 仍在 |

**速率这一列不是表里的真实频率，是按 DDR 代际查表给的标准值。**

判断依据只有一个字节：记录里的 **`m_type` 低字节**（1 DDR1 … 8 LPDDR5X），**一个寄存器都没参与**。所以窗口里那个勾选框现在改名叫「按速率排序（代际标称，非实测）」—— 原来叫「按时序排序」是名不副实的。

**为什么不用 ACTIM / ACTIME 反推频率：**

1. **它是时序（时钟周期数），不是频率。** 要拿它算频率，得先知道这颗颗粒 JEDEC 的 tRCD/tRP 是多少 ns，而表里没有 ns —— 等于两个未知数解一个方程。
2. **位域没有公开定义。** `emi_structures.h` 里只有 `union { quint DRAMC_ACTIME_UNION[8]; quint AcTimeEMI[8]; };`，一个字段名都没有；不同 SoC 代（6765 / 6769 / 6833 / 6873）位域还不一样。
3. **这些值是 SoC 默认值，不随颗粒变。** `ACTIM=0x44584493 / ACTIM1=0x11000D20 / ACTIM05T=0x04002600` 这一组在 **62 条记录**里原样重复，跨品牌、跨容量都一样，说明它跟颗粒频率无关。
4. **覆盖不全。** 278 条记录里 213 条有时序值，但 v30+ 的 8 个 ACTIME 有 45 条全 0（k71v1 那 3 条就是全 0），v11 那 20 条也全 0。

**唯一像"频率"的线索（只是线索，没当结论用）**：v30+ 里 ACTIME0 非零的 20 条记录中，低 16 位有 17 条是 `0x3A5`(933)、2 条是 `0x320`(800)，正好是 LPDDR3 的两种时钟；但有 1 条是 `0x640`(1600) 对不上。样本太少还有反例，所以详情里只把原始值回显出来（标着"可能对应 DRAM 时钟 MHz，未证实"），既不拿它当速率、也不参与排序。

想要真实频率只有三条路：preloader 里 EMI 表**之外**的 DRAMC 频率表、在跑起来的机器上抓（`dmesg | grep -i dramc`、`/proc/dramc`）、或者查颗粒规格书。


### 介质判定规则（uMCP 的坑）

三星的 uMCP 就是 **UFS + LPDDR** 的组合，`KM2H7001CM-B518` = UFS 2.1 64GB + LPDDR4X。
反过来，eMMC 版 eMCP 的料号也有 `KM2*`、`KM3*` 开头的情况，靠料号前缀猜介质一定出错。

所以判定不猜料号，只看 **ID 字段的格式**：

- ID 是 **ASCII 料号**（`KM2H7001CM-B518`、`H9HQ15AECMBDAR`）→ 只能是 **UFS/uMCP**，因为 eMMC 的 CID 不可能是可读字符串
- ID 是 **二进制 CID**（首字节 MID：`0x15` 三星、`0x90` SK Hynix、`0x13` Micron…）→ **eMMC**
- `idlen = 0` 的通配记录没有 ID，才退回看封装字节：`0x3xx`（uMCP）按 UFS 算，其余按 eMMC

纯 UFS 记录（v46/v49/v51）里另有一个 16 位的厂商号，对应 `emi_structures.h` 的 `UFS_VENDOR_*`：
`0x1CE` 三星、`0x1AD` SK Hynix、`0x198` 东芝/铠侠、`0x12C`/`0x02C` 美光。

### eMMC CID 的厂商号（MID）对照表

| MID | 厂商 | MID | 厂商 |
|---|---|---|---|
| 0x01 | Panasonic | 0x88 | YMTC / 长江存储 |
| 0x02 / 0x03 | SanDisk | 0x90 | SK Hynix |
| 0x04 | ISOCOM | 0xD6 | Longsys / 江波龙（FORESEE） |
| 0x11 | Toshiba / Kioxia | 0xDF | SCY / 时创意 |
| 0x13 / 0x2C | Micron | 0xEA | SiliconGo / 硅格 |
| 0x15 | Samsung | 0xF4 | **BIWIN / 佰维** |
| 0x45 | SanDisk / WD | 0xFE | Micron / Numonyx |
| 0x70 | Kingston | 其他 | 显示 `未知(0xNN)`，不猜 |

表里没有的 MID 会原样显示成 `未知(0xF4)` 这种，方便你把新遇到的编号补进 `main.go` 的 `midVendor`。
（`0x88` 有资料写 Micron，实测样机多是长江存储颗粒，这里按 YMTC 标。）
（261117补一句，0xA8是力勤 LEAHKINN）这个没加上去，后续版本再改

CID 第 10 字节是 **PRV**（规范版本），现在会顺带译出来：`0x05` eMMC 4.41、`0x06` 4.51、
`0x07` 5.0、`0x08` 5.1，其它值只显示原值。

### 平台（SoC）是怎么认出来的

左上角有一行 `平台`，写的是"这颗 preloader 大概属于哪个 SoC"。这是**猜的**，界面上总会带
"不一定准" 三个字，报告和 JSON 里也带 `confidence` 字段，别拿它当结论。

按可信度从高到低三条证据：

1. **文件里出现 SoC 型号串**（如 `MT6765` / `MT6771`）：统计全文出现次数，≥2 次标 `较可信`，
   只出现 1 次标 `推测`。
   - `MT63xx` 是 PMIC（MT6357/6358/6311/6328…）、`MT66xx` 是 WiFi/BT/FM 组合芯片，都不是 SoC，已排除；
   - **`MT6752` 这个串几乎每份 preloader 里都出现一次**（公共库残留，实测 6582/6572 的机器也有），
     只出现 1 次时直接丢掉，只在没别的线索时提示一句"只有 1 处 MT6752，不足为据"。
2. **名字里的 4 位型号数字**：`preloader_tb8735ma1_bsp.bin` → `MT8735`、`6761` → `MT6761`。
3. **项目代号对照表**（`main.go` 里的 `platCode`）：`k37mv1` → MT6735/MT6737、`k50v1` → MT6755、
   `k53` → MT6753、`k61v1` → MT6761、`k62v1` → MT6762/MT6765、`k63v2` → MT6763、`k71v1` → MT6771、
   `k80` → MT6580。这个表只收实测见过的代号，拿不准的写两个候选；不在表里的代号会显示
   `未识别 · 项目代号 k388（不在对照表里）`，不会硬套。

实测 48 个样本：k61v1→MT6761、k62v1→MT6765、k63v2→MT6763、k71v1→MT6771 是从文件里的
`MTxxxx` 串直接读出来的；k80 系列文件里只有 `MT6752` 残串，所以走代号表标"推测"。

---

## 二、用法

### 图形界面（默认）

双击 `MTKEmiParser.exe` → 出现三块式窗口：

```
MTK EMI 解析器  v1.1.0  ·  数据库 2026.10.23     ← 标题栏：软件版本 + 数据库版本
┌ 菜单栏 ──────────────────────────────────────────────────────────┐
│ 文件(F)   数据库(D)   帮助(H)                                     │
├ 工具条 ──────────────────────────────────────────────────────────┤
│ [选择文件…] [清空] [复制详情]                                     │
├ 左上 当前文件 ──────────┬ 右侧 详情 ────────────────────────────┤
│ preloader 名 / 平台 /    │ 选中那颗闪存的全部字段：              │
│ 目录 / 大小 / 表版本 /   │ PNM、丝印型号、eMMC 版本、实测速度、  │
│ 记录数 / DDR·介质·封装·  │ 厂商、容量、组成、rank、              │
│ 厂商·容量 汇总           │ 时序寄存器、所在表偏移与文件名        │
├ 左下 闪存支持列表 ──────┤                                       │
│ [ ] 按速率排序（代际标称）│                                       │
│ #0  KM2V7001CM-B706     │                                       │
│     Samsung · uMCP · …  │                                       │
└ 状态栏（解析结果 / 数据库状态）──────────────────────────────────┘
```

- **菜单**：`文件`（打开 / 清空 / 退出）、`数据库`（数据库信息 / 修改内置数据库（要密码）/ 重新载入 / 打开数据库所在目录）、`帮助`（关于 / 版本 / 校验码）
- **左上**是当前文件（选中哪条颗粒，就显示它属于哪个文件）的目录、大小、表版本和汇总
- 左上第 3 行是**文件里头写着的 preloader 名**（EMI 表头后那段字符串）：文件被改名成 `boot1.bin` / `PRELOADER.img` 也能认出它其实是哪份 `preloader_xxx.bin`
- 左上第 4 行是**平台（SoC）**，后面跟一句依据和"不一定准"；猜不出来就写 `未识别 · 项目代号 k80（不在对照表里）`
- 表被截断时左上会多一行 `提示`，例如 `⚠ 表尾缺少记录长度字段（可能被截断或单独提取），按 v38 常见宽度 160 字节/条解析`
- **左下**是闪存支持列表，一颗一行：第一行 `#序号 PNM`，第二行 `厂商 · 封装 · 介质 · DDR版本 · 组成`，右侧是容量和标称速率
- 文本报告的总览表也把 **PNM 单列放在「厂商」前面**（以前只有 CID 里带 PNM，不点进详情看不见）
- **默认按 preloader 里的原始顺序**（表里第几条就是第几条，不做任何排序）
- 勾选「按速率排序（代际标称，非实测）」→ 按速率从快到慢，同速率按容量从大到小
- 点左边任意一条，右边整块换成那条的详情；「复制详情」把右栏文本丢进剪贴板
- 右侧详情按 `类型 → 厂商 → PNM → 丝印型号 → eMMC版本 → CID → ID → 实测速度 → 推荐指数 → 常见年份与设备 → 备注 → 料号/DRAM/容量/组成/rank` 排，其中丝印型号往下那几行都是型号库给的；标签列加粗加深，值用正文色
- 一次拖多个文件时，列表里会插文件分组行；勾上排序后分组行取消，所有颗粒混排
- 拖文件 / 拖整个设备目录 / 多选 / 后台解析 / 自动写 `<文件名>.emi.txt` 都和以前一样

解析在后台线程跑，拖几百 MB 的 `PRELOADER.img` 也不会把窗口卡死。

### 命令行（带任何开关时自动切换）

```
MTKEmiParser.exe preloader_xxx.bin
MTKEmiParser.exe a.bin b.bin c.img       一次解析多个
MTKEmiParser.exe -v preloader_xxx.bin    看全部寄存器 + MR
MTKEmiParser.exe -sort cap x.bin         按容量排序（speed / cap / idx / type）
MTKEmiParser.exe -csv x.bin              另存 x.emi.csv
MTKEmiParser.exe -json x.bin             结构化输出到标准输出
MTKEmiParser.exe -outdir D:\out x.bin    报告写到指定目录
MTKEmiParser.exe -outdir same x.bin      报告和源文件放一起
MTKEmiParser.exe -nopause x.bin          结束不等待按键（脚本里用）
```

判断规则很简单：**命令行里有没有 `-xxx` 开关**。没有开关 = 开窗口；有开关 = 走命令行。

报告默认写在 **exe 所在目录**，不是源文件旁边 —— 就是怕手滑往备份盘里写东西。`-noout` 关掉报告。

`-guitest <输出文件>` 是自检模式：不开窗，把结果框里的内容原样写出来，用来验证界面渲染。
`-guitest <输出文件> -droptest edit|main <文件>` 更进一步：在进程内构造一个真的 `HDROP`，投递到文本框（`edit`）或窗口空白处（`main`），走完整的拖放链路再导出结果 —— 用来回归拖放，不依赖人手拖。
`-dbtest <输出文件>` 也是自检：自动跑一遍密码框（错密码 / 对密码）和数据库编辑器（载入全文 → 保存 → 重新载入），把结果写出来，用来回归第九节那套流程。

### 界面实现上踩过的坑

1. **`WM_DROPFILES` 的 HDROP 在 `wParam` 里，不是 `lParam`。** 传错的话编译运行都不报错，就是拖进来没反应。
2. **子控件会把拖放吞掉。** 占满客户区的 EDIT 自己不是拖放目标时，拖到文本区上的整个拖放会失败。所以每个子控件都要 `DragAcceptFiles(TRUE)` + `SetWindowLongPtrW` 换掉窗口过程，收到 `WM_DROPFILES` 再转给主窗口。
3. **`syscall.StringToUTF16` 遇到字符串里的 NUL 会 panic。** 文件对话框的 filter 本身是双 NUL 结尾的串，用它会直接把进程打死（GUI 版没控制台，看起来就是"点一下就没了"）。必须自己转 UTF-16。
4. 回调用 `runtime.LockOSThread()` 锁线程，否则偶发死锁；消息循环不能用阻塞的 `GetMessageW`（阻塞时 `SendMessage` 进不来，窗口会被系统判为无响应），用 `PeekMessage` + 短休眠。
5. 多行 EDIT 只认 CRLF，正文里的 `\n` 要先转 `\r\n`，否则整篇挤成一行。
6. 字体要实测"4 个西文字符宽 == 2 个汉字宽"（宋体/新宋体在中文系统里 `GetTextFace` 返回的名字还不一样），否则表格对不齐。
7. 回调里的 panic 默认会让整个进程直接消失，所以 `WndProc` 和子控件窗口过程都包了 `recover`，异常写进 `%TEMP%\mtkemi_crash.log`。
8. **列表是 owner-draw 的 LISTBOX**（`LBS_OWNERDRAWFIXED` + `LB_SETITEMHEIGHT`），行高必须跟着 DPI 缩放（`S(48)`），写死像素在 150% 缩放下两行字会互相压。自绘时 `WM_DRAWITEM` 里的 `itemID` 才是行号，不是 `lParam`。
9. **富文本控件要先 `LoadLibrary("Msftedit.dll")` 再 `CreateWindowEx("RICHEDIT50W")`**，顺序反了建出来的是普通 EDIT，`EM_SETCHARFORMAT` 全部无效（加粗和上色都不生效，但不报错）。
10. 卡片圆角是 `SetWindowRgn` 套在子控件上的，**每次 `WM_SIZE` 都要重新建 region**（旧 region 会被系统接管释放），否则拉大窗口后子控件还是圆的旧尺寸。
11. 详情栏用**新宋体**而不是 Consolas：Consolas 没有汉字，中文走字体回退后宽度不受控，`标签 : 值` 的列就对不齐了；新宋体是严格的 2:1 等宽。

---

## 三、`m_type` 编码

`type = (封装 << 8) | DDR代际`，例：

| type | 含义 |
|---|---|
| `0x003` | LPDDR3 / 分离式 |
| `0x006` | LPDDR4X / 分离式 |
| `0x202` | LPDDR2 / eMCP |
| `0x203` | LPDDR3 / eMCP |
| `0x206` | LPDDR4X / eMCP |
| `0x306` | LPDDR4X / uMCP |
| `0x007` | LPDDR5 / 分离式 |

---

## 四、支持的版本与记录布局

| 版本 | 记录长度 | 布局 | rank 字段宽度 |
|---|---|---|---|
| v11 | 168 | `[type][ID16][idlen][dramc0 ×30][rank ×4][MR24B]` | u32 |
| v12 ~ v28 | 188 | `[sub][type][idlen][fwlen][ID16][FW8][dramc0 ×17][rank ×4][MMD][保留][MR24B]` | u32 |
| v30 / v31 | 160 | `[sub][type][idlen][fwlen][ID16][FW8][CONA][CONH][ACTIME ×8][rank ×4]` | **u64** |
| v25（实测变体） | **144** | `[sub][type][idlen][fwlen][ID16][FW8][CONA][CONH][ACTIME ×8][rank ×4]`，无 MR 表 | u32 |
| v32 | 188 | 同 v12 系列 | u32 |
| v35 / v36 / v38 / v39 | 160 | `[sub][type][idlen][fwlen][ID16][FW8][CONA][CONH][ACTIME ×8][rank ×4]`，无 MR 表 | **u64** |
| v46 | 84 | UFS：`[type][idlen][reserved 32B][ufs_id16][rank ×4]` | **u64** |
| v49 | 84 | UFS：`[type][idlen][ufs_id16][rank ×4]` | **u64** |
| v51 | 112 | UFS：`[sub][type][idlen][fwlen][ufs_id16][fw8][CONA][CONH][ACTIME ×8][rank ×4]` | **u64** |

**rank 宽度是个大坑。** `emi_structures.h` 里 `EMIInfoV35/V36/V38/V39` 的 `m_dram_rank_size[4]` 是 `qlong`（8 字节），老版本是 `quint`（4 字节）。按 4 字节读会正好把 4GB 那个 rank 截成高位丢失的 0：一颗 6GB 的 LPDDR4X（`2GB + 4GB`）会被算成 2GB。实测 C1PLUS（MT6789，6GB）就是这样，已修正并核对：8 字节读出来的值全是整数容量（512MB/1GB/1.5GB/2GB/3GB/4GB），`0x1_0000_0000` 在全库出现 11 次。

v30 及之后的这些版本记录里**没有 LPDDR MR 表**（结构体里就没有这个字段），所以详细视图不再硬解出一堆假的 `MR1=0x0000`。

记录长度不写死，靠"记录区尾部那个 u32 == 112 + 条目数×记录长度"反推，同时用记录内容合法性打分，所以记录长度异常的文件也能兜住。

**同一个版本号的布局也可能不一样**：`emi_structures.h` 里 v25 是 160 字节（10 个字 + rank×4 u64 + 48 字节尾），
但实测 `preloader_aus6757_66_m_gomk1.bin` 的 v25 记录是 **144 字节**：10 个字 + rank×4 **u32** @80 + 48 字节尾。
按 160 去读会把 rank 读成垃圾（`0x29656570` 这种），整张表被判成"字段不合理"。
所以现在 `parseRecord` 会拿三种布局依次试：① 版本默认布局 ② `10 字 + rank u32 @80` ③ `10 字 + rank u64 @80`，
用"rank 要么是 0、要么是 128MB 的整数倍"当判据，取第一个合法的（都不合法就退回版本默认）。
同一条规则也让"表尾缺长度字段"时能正确挑出 144 字节/条。

**表尾那串长度字段缺失时也能读**（有些 preloader 只被截出了表头 + 记录，或者在传输/读回时被截断）：
这时退回用"**该版本的常见记录长度**（`emi_structures.h` 的 `emi_len[N] × 4`）+ 逐条记录合法性"重新挑宽度。
表头写的条数超过文件里实际能放下的条数时，按能放下的条数解析，并在窗口/报告里写明
`文件被截断：表头写 5 条，只解析到 3 条完整记录`，不会整张表丢掉报错。
实测样本 `0717_preloader_k61v1_64_bsp_37D5FA1A49.bin` 只有 912 字节（表头 + 5 条记录，没有尾部长度表），
旧版本直接报"记录全部不合法"，现在能正常出 5 条 eMCP 记录。

实测覆盖（本机 48 个样本）：v11 / v12 / v20 / v21 / v22 / v30 / v32 / v35 / v36 / v38 全部正确。
v46 / v49 / v51（纯 UFS）本机没有样本，布局按 MTK 官方 `emi_structures.h` 实现，没有实机验证。

---

## 五、脏数据的处理

- 记录字段不合理（type 越界、DDR 代际字节非法、rank 容量不是 128MB 整数倍）的记录标记为无效，**不进表格**，只在下面一行提示 `⚠ 另有 N 条记录字段不合理已隐藏：#12 #13 ...`（窗口里显示为 `!`）
- 一整张表里过半记录都不合法 → 判定为镜像里残留数据误命中，整张表丢掉，文件头提示"另有 N 处标记是残留数据误命中，已忽略"
- 表被截断（缺尾部长度表 / 记录区被切掉一截）→ 能读多少读多少，标题行写 `条目 3/5（文件被截断）`，下面提示一句，不再整个文件报错
- 表头之后连一条完整记录都没有 → 才报错，并说明"不够一条 v38 记录（160 字节），文件被截断在记录中间"
- 老 preloader 里 MR 表偶尔写同一个 MR 号两次（表本身的问题），显示成 `MR1=0x00(表内重复)`，不是解析错
- 展锐/瑞芯微/全志的固件没有这张表，会直接告诉你"不是 MTK 平台的 preloader"

---

## 六、只认这些签名

```
MTK_BLOADER_INFO_v20\0
        ↑ 27 字节表头        61 字节 preloader 名 | 12 字节校验 | 8 字节 "MTK_BIN" | 条目数
```

注意表头里版本号前有下划线，`_v20`，不是 `V20`。老版本还有 `_EMMC` / `_UFS` 后缀，解析器两种都吃。

---

## 七、编译

```
go build -trimpath -ldflags "-s -w -H=windowsgui" -o MTKEmiParser.exe .
```

- Go 1.21+，仅标准库，完全离线
- `-H=windowsgui` 去掉黑色控制台窗口；从 cmd 带开关运行时输出照样能重定向（子进程继承句柄）
- 想要带控制台的版本就去掉 `-H=windowsgui`

---

## 八、文件

```
main.go              解析 + 输出 + 报告 + JSON/CSV
pnmdb.go             内置 PNM 型号库：解压、校验、查表、丝印/速度展示
pnmdB.tsv.gz         型号库原始数据（go:embed，首次运行解压成 tsv）
gui_windows.go       Win32 窗口（纯 syscall，无 cgo）：拖放、文件选择、后台解析
gui_dialogs_windows.go  菜单、密码框、数据库编辑器、关于框
gui_other.go         非 Windows 平台空实现（只走命令行）
console_windows.go   Windows 控制台 UTF-8 / VT 初始化
console_other.go     其他平台空实现
go.mod               模块定义，无依赖
MTKEmiParser.exe     编译好的产物
MTKEmiParser.exe.sha256  exe 的 SHA-256
mtk_emi_pnm_db.tsv   首次运行后落盘的明文型号库（可改，见第九节）
```

### 界面实现里的几个坑（改代码前先看）

1. **必须 `runtime.LockOSThread()`**：窗口属于创建它的 OS 线程，不锁线程时 goroutine 会在 syscall 中间被搬走，Windows 直接死锁（表现为偶发卡死，10 次里挂 1 次）。
2. **不能用阻塞的 `GetMessageW`**：Go 线程阻塞在那个 syscall 里时，别的进程 `SendMessage` 过来的消息进不了 WndProc，窗口在系统眼里就是"无响应"。改用 `PeekMessage` + 6ms 休眠。
3. **多行 EDIT 只认 CRLF**：裸 `\n` 不换行，整篇会挤成几行。
4. **字体要挑"西文半宽 + 中文全宽"的**：用 `GetTextExtentPoint32` 量 `WWWW` 和 `中国` 的宽度是否相等来选（新宋体/宋体满足），别按字体名判断（中文系统里 NSimSun 的 `GetTextFace` 返回的是"新宋体"）。
5. **界面里把框线换成 ASCII**：`│ ┌ └ × ≈` 在宋体里会走全角或字库回退，宽度不再是半宽，表格立刻错位；`.emi.txt` 报告里仍保留原来的框线。
6. 解析放在 goroutine 里跑，完成后 `PostMessage(WM_APP+1)` 回 UI 线程贴文本，别在后台线程碰控件。

---

## 九、内置 PNM 型号库（丝印型号 / eMMC 版本 / 实测速度）

preloader 里只有 9 字节 CID 和里面的 6 字节 PNM（`RX14MB`、`hB8aP>`、`S0J9K9`），
看不出**原始丝印料号**、**eMMC 规范版本**、**实测读写速度**。这些查表数据来自
`常见BGA221-eMCP汇总260208.xlsx`（Forward 整理），已经内置进 exe。

### 读内置还是读外置？

**读外置。** 文件就在 exe 同目录：`mtk_emi_pnm_db.tsv`（明文 TSV）。

| 情况 | 行为 |
|---|---|
| 第一次运行，目录里没有这个文件 | 把 exe 里的 gz 解压出来写成 `mtk_emi_pnm_db.tsv`，然后**读这个文件** |
| 之后每次启动 | 直接读 `mtk_emi_pnm_db.tsv`，**不看 exe 里的副本**（除非文件丢了） |
| 文件被删掉 | 下次启动重新解压一份 |
| 文件读不出来 / 目录只读写不进 | 退回用 exe 里的副本，界面显示"内置副本" |
| 文件被改过 | 照用你的，界面显示"本地已修改" |

也就是说：**内置副本只是安装包，日常跑的都是外置那份 TSV。** 改 TSV 存盘后点
「数据库 → 重新载入数据库」就立刻生效，不用重开软件。

### 文件格式

```
# MTK EMI 闪存型号库（PNM 数据库）                ← # 开头是注释
# VERSION 2026.10.23                            ← 这一行会显示在标题栏
PNM	DRAM_GB	EMMC_GB	厂商	厂商中文	丝印型号	年份	CID	eMMC/DDR规格	读写实测	推荐	状态	常见设备	备注
RX14MB	4	32	Samsung	三星	KMRX10014M-B614	2017~2022	150100525831344d42	5.1/1866	HS400 r260w80（0x04）	★★★★		MP4/杂牌mtk平板	已知仅在MP4使用，部分寿命03+
RC14MB	4	64	Samsung	三星	KMRC10014M-B809	2016	150100524331344d42	5.1/1866	HS400 r290w150（0x02）	★★★★		MP4	正常水平，可能存在闪存时序兼容问题
```

（真文件是制表符分隔、14 列，这里为了看得清用空格对齐。）

### 怎么查（PNM + 容量 + CID）

海力士同一个 PNM 会挂好几种容量（`hB8aP>` 有 2+32、3+32），CID 一模一样，
光靠 PNM 分不开，所以按顺序：

1. PNM 只命中一条 → 直接用
2. 多条 → 先用 **EMI 表里读到的 DRAM 容量**筛（机器上的真实容量，最可信）
3. 还多条 → 再用 **CID 前 9 字节**筛
4. 仍然分不开 → 候选全列出来，标"同 PNM 有 N 个候选"

库里只有一颗、但容量对不上时，会明确写"可能不是同一颗"，不硬套。

实测例子：`hB8aP>` 3GB → `H9TQ27ADFTMC`；`hB8aP>` 2GB → 2+32 那条；`HBG4e` 2GB → 两颗都列。

### 显示在哪

- **右侧详情**：`类型 → 厂商 → PNM → 丝印型号 → eMMC版本 → CID → ID → 实测速度 → 推荐指数 → 常见年份与设备 → 备注`，后面接 `料号 / DRAM / 容量 / 组成 / rank`
- **文本报告总览表**：PNM 后面加一列 `丝印型号`（一条都没命中时这一列不出现，表不会无谓变宽）
- **每条记录详情**：同上顺序（报告里是 `厂商 / PNM / 丝印型号 / eMMC版本 / CID 解码 / ID / 实测速度 / 推荐指数 / 常见年份与设备 / 备注 / 匹配依据`）
- **JSON**：`silk` / `emmc_spec` / `speed_measured` / `speed_cond`

### 速度那一行的写法

速度是**读**在前、**写**在后，括号里是**测速条件**，前缀固定写 `测试平台：`：

```
实测速度 : 读 260 写 80 MB/s（测试平台：HS400，寿命 30~40% · 0x04）
实测速度 : 读 150 写 20 MB/s（测试平台：HS200，寿命 10~20% · 0x02，设备端）
实测速度 : 读 150 写 20 MB/s（测试平台：HS200，清零片）
实测速度 : 读 160 写 30 MB/s（测试平台：HS200）
```

- **表格里标了 HS400 的就是 HS400，没标的一律按 HS200 写**
- 括号里的寿命来自表格括号里那个 `0xNN`，**表格没写就空着**，不编造"空盘/半盘"
- 寿命编码按 JEDEC `DEVICE_LIFE_TIME_EST` 一对一换算，不是固定一档：
  `0x01`=0~10%、`0x02`=10~20%、`0x03`=20~30% …… `0x0A`=90~100%、`0x0B`=已超出最大寿命
- 同一颗颗粒空盘和快写满时速度差一大截，所以这个寿命基准不写清楚，速度就没有可比性

### 修改数据库（要密码）

菜单 **数据库(D) → 修改内置数据库…**，默认密码 `forward`：

1. 输密码，错了直接拒绝
2. 弹出的编辑器里就是 `mtk_emi_pnm_db.tsv` 全文（方向键、回车都能用，Enter 不会误保存）
3. 改完点「保存」→ 写回文件 + 立刻重新载入，标题栏、详情同步刷新
4. 「还原内置」把编辑框内容换成 exe 里那份原始数据（不点保存就不影响文件）

改坏了也不怕：解析失败的行会被跳过，整份文件都读不出来就退回内置副本，关于框里会说。

### 版本与校验

- 标题栏：`MTK EMI 解析器  v1.1.0  ·  数据库 2026.10.23`
- 关于框：软件版本、数据库版本/条目数/路径、数据库 SHA-256 前 16 位、exe SHA-256 前 32 位
- 数据库文件与内置副本不一致 → 显示"本地已修改"；一致 → "校验一致"
- exe 的完整 SHA-256 在 `MTKEmiParser.exe.sha256`，可用 `certutil -hashfile MTKEmiParser.exe SHA256` 复核
