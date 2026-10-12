package syncdaily

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// fakeExecer 记录 Exec 调用（单测不连库）。
type fakeExecer struct {
	calls []fakeCall
	err   error
}

type fakeCall struct {
	sql  string
	args []any
}

func (f *fakeExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls = append(f.calls, fakeCall{sql: sql, args: args})
	return pgconn.CommandTag{}, f.err
}

// makeBars 生成 n 行测试 bar（值随序号变化）。
func makeBars(n int) []Bar {
	bars := make([]Bar, 0, n)
	for i := 0; i < n; i++ {
		bars = append(bars, Bar{
			Symbol: "600000.SH", Market: "CN.SH", TradeDate: "2026-09-30",
			Open: Mil(9020 + i), High: Mil(9130), Low: Mil(8920), Close: Mil(9160),
			Volume: int64(1474848 + i), Amount: Mil(1386209920000),
		})
	}
	return bars
}

// TestUpsertBarsChunking >500 行分块提交；SQL 形态与参数逐位断言。
func TestUpsertBarsChunking(t *testing.T) {
	db := &fakeExecer{}
	w := &UpsertWriter{DB: db}
	written, err := w.UpsertBars(context.Background(), makeBars(1200))
	if err != nil {
		t.Fatalf("UpsertBars: %v", err)
	}
	if written != 1200 {
		t.Errorf("written = %d, want 1200", written)
	}
	if len(db.calls) != 3 {
		t.Fatalf("预期 3 个分块(500/500/200), got %d", len(db.calls))
	}
	for i, want := range []int{500, 500, 200} {
		if got := len(db.calls[i].args); got != want*9 {
			t.Errorf("chunk[%d] args = %d, want %d", i, got, want*9)
		}
	}

	c := db.calls[0]
	sql := c.sql
	for _, want := range []string{
		"INSERT INTO kline_daily (symbol, market, trade_date, open, high, low, close, volume, amount, source) VALUES ",
		" ON CONFLICT (symbol, market, trade_date) DO UPDATE SET ",
		"open = EXCLUDED.open", "high = EXCLUDED.high", "low = EXCLUDED.low",
		"close = EXCLUDED.close", "volume = EXCLUDED.volume",
		"amount = EXCLUDED.amount", "source = EXCLUDED.source",
		"'gotdx'",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL 缺 %q:\n%s", want, sql)
		}
	}

	// 第一行参数逐位：symbol/market/date + 4 价 numeric(×1000 定点) + volume(股) + amount
	if c.args[0] != "600000.SH" || c.args[1] != "CN.SH" {
		t.Errorf("args[0:2] = %v, %v", c.args[0], c.args[1])
	}
	date, ok := c.args[2].(pgtype.Date)
	if !ok || !date.Valid || date.Time.Format("2006-01-02") != "2026-09-30" {
		t.Errorf("args[2] = %#v", c.args[2])
	}
	checkMil := func(name string, v any, wantMil int64) {
		n, ok := v.(pgtype.Numeric)
		if !ok || !n.Valid || n.Int == nil || n.Int.Int64() != wantMil || n.Exp != -3 {
			t.Errorf("%s = %#v, want Numeric{Int:%d,Exp:-3,Valid:true}", name, v, wantMil)
		}
	}
	checkMil("open", c.args[3], 9020)
	checkMil("high", c.args[4], 9130)
	checkMil("low", c.args[5], 8920)
	checkMil("close", c.args[6], 9160)
	if vol, ok := c.args[7].(int64); !ok || vol != 1474848 {
		t.Errorf("volume = %#v, want 1474848", c.args[7])
	}
	checkMil("amount", c.args[8], 1386209920000)

	// 第二块起始参数应接着第 501 行
	if got := db.calls[1].args[3].(pgtype.Numeric).Int.Int64(); got != int64(9020+500) {
		t.Errorf("chunk[1] 首 open = %d, want %d", got, 9020+500)
	}
}

// TestUpsertBarsEmpty 空入参直接返回 0，不发 SQL。
func TestUpsertBarsEmpty(t *testing.T) {
	db := &fakeExecer{}
	written, err := (&UpsertWriter{DB: db}).UpsertBars(context.Background(), nil)
	if err != nil || written != 0 || len(db.calls) != 0 {
		t.Errorf("written=%d err=%v calls=%d; want 0,nil,0", written, err, len(db.calls))
	}
}

// TestUpsertBarsError 传播执行错误。
func TestUpsertBarsError(t *testing.T) {
	db := &fakeExecer{err: context.DeadlineExceeded}
	_, err := (&UpsertWriter{DB: db}).UpsertBars(context.Background(), makeBars(1))
	if err == nil {
		t.Error("应传播错误")
	}
}

// TestMilNumeric 定点 → pgtype.Numeric 精确无损。
func TestMilNumeric(t *testing.T) {
	n := milNumeric(Mil(9160))
	if n.Int.Int64() != 9160 || n.Exp != -3 || !n.Valid {
		t.Errorf("milNumeric(9160) = %#v", n)
	}
	// 数值语义 = 9160×10⁻³ = 9.160（与 Mil.String 渲染一致）
	if Mil(9160).String() != "9.16" {
		t.Errorf("Mil String = %q", Mil(9160).String())
	}
}
