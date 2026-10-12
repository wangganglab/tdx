package syncdaily

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Source kline_daily.source 列写入值。
const Source = "gotdx"

// upsertChunk 单条 INSERT 的行数上限：10 列中 9 个绑定参数 × 500 = 4500 个，
// 远低于 PG 65535 绑定参数上限。
const upsertChunk = 500

// dbExecer pgxpool.Pool 满足的最小执行接口（单测注入用）。
type dbExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// UpsertWriter kline_daily 幂等写入器（INSERT ... ON CONFLICT DO UPDATE）。
type UpsertWriter struct {
	DB dbExecer
}

// NewPGWriter 连接 PG（pgxpool，Ping 确认可用）。
func NewPGWriter(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("PG DSN 解析失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("PG 连接失败(%s): %w", dsn, err)
	}
	return pool, nil
}

// UpsertBars 分块批量幂等写入 kline_daily，返回提交行数
// （等价 python upsert_bars：返回提交的行数；空入参直接返回 0）。
func (w *UpsertWriter) UpsertBars(ctx context.Context, bars []Bar) (int, error) {
	for start := 0; start < len(bars); start += upsertChunk {
		end := start + upsertChunk
		if end > len(bars) {
			end = len(bars)
		}
		if err := w.execUpsert(ctx, bars[start:end]); err != nil {
			return start, err
		}
	}
	return len(bars), nil
}

// execUpsert 写一块（≤upsertChunk 行）。
func (w *UpsertWriter) execUpsert(ctx context.Context, chunk []Bar) error {
	var sql bytes.Buffer
	args := make([]any, 0, len(chunk)*9)
	sql.WriteString("INSERT INTO kline_daily " +
		"(symbol, market, trade_date, open, high, low, close, volume, amount, source) VALUES ")
	for i, b := range chunk {
		if i > 0 {
			sql.WriteString(", ")
		}
		base := i*9 + 1
		sql.WriteString("(")
		for p := 0; p < 9; p++ {
			if p > 0 {
				sql.WriteString(",")
			}
			sql.WriteString("$" + strconv.Itoa(base+p))
		}
		sql.WriteString(",'gotdx')")
		date, err := parseTradeDate(b.TradeDate)
		if err != nil {
			return err
		}
		args = append(args,
			b.Symbol, b.Market, date,
			milNumeric(b.Open), milNumeric(b.High), milNumeric(b.Low), milNumeric(b.Close),
			b.Volume, milNumeric(b.Amount),
		)
	}
	sql.WriteString(" ON CONFLICT (symbol, market, trade_date) DO UPDATE SET " +
		"open = EXCLUDED.open, high = EXCLUDED.high, low = EXCLUDED.low, close = EXCLUDED.close, " +
		"volume = EXCLUDED.volume, amount = EXCLUDED.amount, source = EXCLUDED.source")
	_, err := w.DB.Exec(ctx, sql.String(), args...)
	if err != nil {
		return fmt.Errorf("upsert kline_daily(%d 行): %w", len(chunk), err)
	}
	return nil
}

// milNumeric Mil（×1000 定点）→ pgtype.Numeric，精确无损：9160×10⁻³ = 9.160。
func milNumeric(m Mil) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(int64(m)), Exp: -3, Valid: true}
}

// parseTradeDate "2026-09-28" → pgtype.Date。
func parseTradeDate(s string) (pgtype.Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return pgtype.Date{}, fmt.Errorf("trade_date 非法 %q: %w", s, err)
	}
	return pgtype.Date{Time: t, Valid: true}, nil
}
