// cmd/syncdaily 全市场日线同步 CLI — tdx 本地网关 → PostgreSQL kline_daily。
//
// 替代 OmniQuant apps/data 的 `omniquant-data data sync-daily`（python 实现），
// 行为等价（单位换算/分页/universe/输出格式），输出供 daily-loop 日志消费：
//
//	Universe: tdx 网关全市场
//	gotdx 日线同步: N 只 × M 根, workers=K
//	gotdx 日线同步完成: 成功 X 只 / 写入 Y 行 / 失败 Z 只
//
// 用法:
//
//	syncdaily                                          # 网关全市场, 45 自然日
//	syncdaily -symbols 600000.SH,000001.SZ             # 指定代码
//	syncdaily -days-back 90 -workers 8                 # 加大回看窗口/并发
//	syncdaily -dsn postgresql://user:pass@host/db      # 缺省取 env DATABASE_URL
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/injoyai/tdx/syncdaily"
)

const (
	defaultDSN        = "postgresql://postgres:123456@127.0.0.1:5432/omniquant"
	maxFailedPrintN   = 20 // 失败列表最多打印条数，等价 python _MAX_FAILED_PRINT
	gatewayDownFormat = "tdx 网关不可达(%s 健康检查失败): 先启动 tdx 网关(scripts/launchd/com.omniquant.tdx-gateway.plist)"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		daysBack    int
		symbolsFlag string
		workers     int
		dsnFlag     string
	)
	flag.IntVar(&daysBack, "days-back", 45, "回看自然日窗口(end=今天)")
	flag.StringVar(&symbolsFlag, "symbols", "", "逗号分隔带后缀代码(如 600000.SH,000001.SZ);缺省=网关全市场")
	flag.IntVar(&workers, "workers", 8, "并发拉取数")
	flag.StringVar(&dsnFlag, "dsn", "", "PG 连接串;缺省取 env DATABASE_URL,再缺省 "+defaultDSN)
	flag.Parse()
	ctx := context.Background()

	dsn := dsnFlag
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		dsn = defaultDSN
	}
	gwURL := os.Getenv("OMNIQUANT_TDX_GATEWAY")
	if gwURL == "" {
		gwURL = syncdaily.DefaultGatewayURL
	}

	// 健康检查（GET /，3s 超时），网关不可达 → 报错退出码 1
	gw := syncdaily.NewGateway(gwURL)
	if !gw.Available(ctx) {
		fmt.Fprintf(os.Stderr, gatewayDownFormat+"\n", gwURL)
		return 1
	}

	var symbols []string
	if list := parseSymbols(symbolsFlag); len(list) > 0 {
		symbols = list
	} else {
		fmt.Println("Universe: tdx 网关全市场")
		u, err := gw.StockUniverse(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "获取全市场代码失败: %v\n", err)
			return 1
		}
		symbols = u
	}

	total := syncdaily.DaysToBars(daysBack)
	fmt.Printf("gotdx 日线同步: %d 只 × %d 根, workers=%d\n", len(symbols), total, workers)

	batch := syncdaily.FetchBatch(ctx, gw, symbols, total, workers)

	pool, err := syncdaily.NewPGWriter(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	defer pool.Close()

	written, err := (&syncdaily.UpsertWriter{DB: pool}).UpsertBars(ctx, batch.Bars)
	if err != nil {
		fmt.Fprintf(os.Stderr, "写入 kline_daily 失败: %v\n", err)
		return 1
	}

	fmt.Printf("gotdx 日线同步完成: 成功 %d 只 / 写入 %d 行 / 失败 %d 只\n",
		batch.OK, written, len(batch.Failed))
	if n := len(batch.Failed); n > 0 {
		fmt.Printf("失败 %d 只(前 20):\n", n)
		for i, f := range batch.Failed {
			if i >= maxFailedPrintN {
				break
			}
			fmt.Printf("  - %s\n", f)
		}
	}
	return 0
}

// parseSymbols "600000.SH, 000001.SZ" → []string（去空白、去空项，
// 等价 python _parse_symbols）。
func parseSymbols(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
