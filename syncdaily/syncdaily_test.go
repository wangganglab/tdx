package syncdaily

import (
	"testing"
)

// 真实响应样例（600000.SH 最近 2 根，2026-10-10 实测裁剪；oracle =
// OmniQuant apps/data tests/unit/test_gotdx_gateway.py REAL_KLINE_RESPONSE）。
// 数值已与库内逐位核对：首根 Last=0 是历史 bar 常态，close 必须取 Close。
const realKlineJSON = `{"code":0,"msg":"ok","data":{"Count":2,"List":[
 {"Last":0,"Open":9020,"High":9130,"Low":8920,"Close":9020,"Order":0,"Volume":1474848,"Amount":1386209920000,"Time":"2026-09-30T15:00:00+08:00","UpCount":0,"DownCount":0},
 {"Last":9480,"Open":9450,"High":9710,"Low":9410,"Close":9540,"Order":0,"Volume":947958,"Amount":915309824000,"Time":"2026-10-09T15:00:00+08:00","UpCount":0,"DownCount":0}]}}`

// TestMilString 换算逐位断言：÷1000 定点渲染（等价 python Decimal(v)/1000）。
func TestMilString(t *testing.T) {
	cases := []struct {
		in   Mil
		want string
	}{
		{9160, "9.16"},   // 收盘价 9.16
		{9030, "9.03"},   // 开盘价 9.03
		{9000, "9"},      // 整数价（python Decimal 归一化去尾零 → "9"）
		{9020, "9.02"},   // 开盘价 9.02
		{9710, "9.71"},   // 最高价 9.71
		{5, "0.005"},     // 小数补零
		{0, "0"},
		{1386209920000, "1386209920"}, // amount ÷1000 = 1386209920 元（13.86 亿）
		{915309824000, "915309824"},
		{-5, "-0.005"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("Mil(%d).String() = %q, want %q", int64(c.in), got, c.want)
		}
	}
}

// TestToBar 真实响应 fixture 逐位断言：价格/amount ÷1000、volume 手→股、
// Last=0 时 close 取 Close。
func TestToBar(t *testing.T) {
	kl, err := unmarshalKlines(realKlineJSON)
	if err != nil {
		t.Fatalf("fixture 解析失败: %v", err)
	}
	if len(kl) != 2 {
		t.Fatalf("预期 2 根, got %d", len(kl))
	}

	b0, err := ToBar("600000.SH", kl[0])
	if err != nil {
		t.Fatalf("ToBar: %v", err)
	}
	want0 := Bar{
		Symbol: "600000.SH", Market: "CN.SH", TradeDate: "2026-09-30",
		Open: 9020, High: 9130, Low: 8920, Close: 9020, // Last=0，close 取 Close=9020
		Volume: 1474848 * 100, Amount: 1386209920000,
	}
	if b0 != want0 {
		t.Errorf("bar0 = %+v, want %+v", b0, want0)
	}
	// 渲染层逐位
	if got := b0.Open.String() + "/" + b0.High.String() + "/" + b0.Low.String() + "/" + b0.Close.String(); got != "9.02/9.13/8.92/9.02" {
		t.Errorf("bar0 价格 = %q, want 9.02/9.13/8.92/9.02", got)
	}
	if got := b0.Amount.String(); got != "1386209920" {
		t.Errorf("bar0 amount = %q, want 1386209920", got)
	}

	b1, _ := ToBar("600000.SH", kl[1])
	if b1.TradeDate != "2026-10-09" || b1.Volume != 94795800 {
		t.Errorf("bar1 = %+v", b1)
	}
	if got := b1.Close.String() + "/" + b1.Amount.String(); got != "9.54/915309824" {
		t.Errorf("bar1 close/amount = %q, want 9.54/915309824", got)
	}
}

// TestToBarMarket 后缀 → market 推导。
func TestToBarMarket(t *testing.T) {
	k := Kline{Open: 1000, Close: 1000, Time: "2026-10-09T15:00:00+08:00"}
	for sym, market := range map[string]string{"000001.SZ": "CN.SZ", "600000.SH": "CN.SH"} {
		b, err := ToBar(sym, k)
		if err != nil {
			t.Fatalf("%s: %v", sym, err)
		}
		if b.Market != market {
			t.Errorf("%s market = %q, want %q", sym, b.Market, market)
		}
	}
	if _, err := ToBar("830001.BJ", k); err == nil {
		t.Error("BJ 应报错")
	}
	if _, err := ToBar("600000", k); err == nil {
		t.Error("无后缀应报错")
	}
}

// TestSplitSymbol 代码转换 600000.SH ↔ 600000。
func TestSplitSymbol(t *testing.T) {
	code, suffix, err := SplitSymbol("600000.SH")
	if err != nil || code != "600000" || suffix != "SH" {
		t.Errorf("600000.SH → %q,%q,%v", code, suffix, err)
	}
	code, suffix, err = SplitSymbol("000001.SZ")
	if err != nil || code != "000001" || suffix != "SZ" {
		t.Errorf("000001.SZ → %q,%q,%v", code, suffix, err)
	}
	// 小写后缀容忍（python suffix.upper() 等价）
	if code, _, err := SplitSymbol("600000.sh"); err != nil || code != "600000" {
		t.Errorf("600000.sh → %q,%v", code, err)
	}
	for _, bad := range []string{"600000", "830001.BJ", "600000.HK", "600000.XS"} {
		if _, _, err := SplitSymbol(bad); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
	if got, err := GatewayCode("600000.SH"); err != nil || got != "600000" {
		t.Errorf("GatewayCode = %q,%v", got, err)
	}
}

// TestFromGatewayItem universe 项转换：sh/sz 转换，bj 跳过。
func TestFromGatewayItem(t *testing.T) {
	for item, want := range map[string]string{
		"sh600000": "600000.SH",
		"sz000001": "000001.SZ",
		"sh688166": "688166.SH",
		"sz300750": "300750.SZ",
	} {
		if got, ok := FromGatewayItem(item); !ok || got != want {
			t.Errorf("FromGatewayItem(%q) = %q,%v; want %q", item, got, ok, want)
		}
	}
	for _, skip := range []string{"bj830001", "sh", ""} {
		if got, ok := FromGatewayItem(skip); ok {
			t.Errorf("FromGatewayItem(%q) = %q 应跳过", skip, got)
		}
	}
}

// TestDaysToBars 等价 python max(10, days_back * 40 // 45)。
func TestDaysToBars(t *testing.T) {
	cases := []struct{ days, want int }{
		{45, 40}, {90, 80}, {5, 10}, {10, 10}, {1, 10}, {100, 88}, {0, 10},
	}
	for _, c := range cases {
		if got := DaysToBars(c.days); got != c.want {
			t.Errorf("DaysToBars(%d) = %d, want %d", c.days, got, c.want)
		}
	}
}
