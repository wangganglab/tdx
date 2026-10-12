// Package syncdaily 全市场日线同步 — tdx 本地网关 → PostgreSQL kline_daily。
//
// 替代 OmniQuant apps/data 的 python 实现（omniquant-data data sync-daily），
// 行为与其逐位等价（oracle = omniquant_data/southbound/feeds/gotdx_gateway.py）：
//   - 单位换算：价格/amount ÷1000、volume 手→股（×100）
//   - 分页：单请求上限 800 根，start=0 最新一页向历史翻页，页不足即停
//   - universe：GET /code/stocks，sh/sz → 600000.SH 形态，bj 跳过
package syncdaily

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// PageLimit 单请求 K 线根数上限（网关限制："单次数量不能超过800"）
	PageLimit = 800
	// VolumeLot 网关 volume 单位是手；kline_daily 约定为股
	VolumeLot = 100
)

// Mil 定点小数：网关价格/成交额字段 = 实际值 ×1000（单位 0.001）。
// 用 int64 定点而非 float64，保证 ÷1000 逐位无损（等价 python Decimal(v)/1000）。
type Mil int64

// String 精确十进制渲染 ÷1000 后的值。9160→"9.16"、9000→"9"、5→"0.005"。
func (m Mil) String() string {
	neg := m < 0
	u := uint64(m)
	if neg {
		u = uint64(-m)
	}
	s := strconv.FormatUint(u, 10)
	var str string
	if len(s) <= 3 {
		str = "0." + strings.Repeat("0", 3-len(s)) + s
	} else {
		str = s[:len(s)-3] + "." + s[len(s)-3:]
	}
	str = strings.TrimRight(str, "0")
	str = strings.TrimSuffix(str, ".")
	if neg {
		str = "-" + str
	}
	return str
}

// Bar 一行 kline_daily（已换算为库内单位）。
type Bar struct {
	Symbol    string // 600000.SH
	Market    string // CN.SH / CN.SZ
	TradeDate string // 2026-09-28
	Open      Mil
	High      Mil
	Low       Mil
	Close     Mil
	Volume    int64 // 股（网关手 ×100）
	Amount    Mil
}

// Kline 网关 /kline/day List 元素（原始字段，未换算）。
// Last 历史 bar 恒 0 不可用，close 取 Close（见 ToBar）。
type Kline struct {
	Open   int64  `json:"Open"`
	High   int64  `json:"High"`
	Low    int64  `json:"Low"`
	Close  int64  `json:"Close"`
	Volume int64  `json:"Volume"` // 手
	Amount int64  `json:"Amount"`
	Time   string `json:"Time"` // 2026-09-28T15:00:00+08:00
}

// ToBar 网关原始 bar → kline_daily 行（换算逐位等价 python _parse_bars）。
func ToBar(symbol string, k Kline) (Bar, error) {
	_, suffix, err := SplitSymbol(symbol)
	if err != nil {
		return Bar{}, err
	}
	if len(k.Time) < 10 {
		return Bar{}, fmt.Errorf("kline Time 非法: %q", k.Time)
	}
	return Bar{
		Symbol:    symbol,
		Market:    "CN." + suffix,
		TradeDate: k.Time[:10],
		Open:      Mil(k.Open),
		High:      Mil(k.High),
		Low:       Mil(k.Low),
		Close:     Mil(k.Close), // ⚠️ Last 历史 bar 恒 0，不可用
		Volume:    k.Volume * VolumeLot,
		Amount:    Mil(k.Amount), // ⚠️ amount 与价格同样 ÷1000 才是元
	}, nil
}

// DaysToBars 自然日窗口 → 日 K 根数（45 自然日 ≈ 35 根交易日，取 40 根余量）。
// 等价 python: max(10, days_back * 40 // 45)。
func DaysToBars(daysBack int) int {
	n := daysBack * 40 / 45
	if n < 10 {
		n = 10
	}
	return n
}

// SplitSymbol 带后缀代码 → (裸代码, 大写后缀)。
// 无后缀拒绝（无法定市场，且会撞 kline_daily 的 symbol 带后缀 CHECK 约束）；
// .BJ 网关不支持；仅 SH/SZ。等价 python to_gateway_code。
func SplitSymbol(symbol string) (code, suffix string, err error) {
	dot := strings.LastIndex(symbol, ".")
	if dot < 0 {
		return "", "", fmt.Errorf("symbol must have exchange suffix (e.g. 600000.SH): %s", symbol)
	}
	code, suffix = symbol[:dot], strings.ToUpper(symbol[dot+1:])
	if suffix == "BJ" {
		return "", "", fmt.Errorf("BJ not supported by gotdx gateway: %s", symbol)
	}
	if suffix != "SH" && suffix != "SZ" {
		return "", "", fmt.Errorf("unsupported exchange suffix: %s", symbol)
	}
	return code, suffix, nil
}

// GatewayCode 带后缀代码 → 网关裸代码。600000.SH → "600000"。
func GatewayCode(symbol string) (string, error) {
	code, _, err := SplitSymbol(symbol)
	return code, err
}

// FromGatewayItem 网关 universe 项 → 带后缀代码。sh600000 → "600000.SH"；
// bj 前缀（网关日 K 不支持）返回 false 跳过。等价 python fetch_stock_universe。
func FromGatewayItem(item string) (string, bool) {
	if len(item) <= 2 {
		return "", false
	}
	switch prefix, code := item[:2], item[2:]; prefix {
	case "sh":
		return code + ".SH", true
	case "sz":
		return code + ".SZ", true
	default: // bj：网关日 K 不支持，跳过
		return "", false
	}
}
