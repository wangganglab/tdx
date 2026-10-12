package syncdaily

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultGatewayURL 本地 tdx 网关缺省地址（launchd 常驻 8080）。
const DefaultGatewayURL = "http://127.0.0.1:8080"

const (
	requestTimeout = 10 * time.Second // 单请求超时，等价 python _TIMEOUT
	healthTimeout  = 3 * time.Second  // 健康检查超时，等价 python _HEALTH_TIMEOUT
	defaultWorkers = 8                // 等价 python fetch_daily_batch max_workers 缺省
)

// Gateway tdx 本地网关 HTTP 客户端（extend/httpserver 提供服务）。
type Gateway struct {
	BaseURL string
	HTTP    *http.Client
}

// NewGateway 构造网关客户端。显式 Proxy=nil：本机系统代理会劫持请求
// （等价 python 模块入口 _disable_proxy()）。
func NewGateway(baseURL string) *Gateway {
	return &Gateway{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: requestTimeout, Transport: &http.Transport{Proxy: nil}},
	}
}

// envelope 网关统一响应 {"code":0,"msg":"ok","data":...}。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// getJSON GET 并校验 HTTP 200；业务码（code!=0）由调用方按需校验。
func (g *Gateway) getJSON(ctx context.Context, path string, query url.Values) (envelope, error) {
	u := g.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return envelope{}, err
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return envelope{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return envelope{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return envelope{}, fmt.Errorf("gateway HTTP %d: %s %s", resp.StatusCode, path, query.Encode())
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return envelope{}, fmt.Errorf("gateway 响应解析失败: %s %s: %w", path, query.Encode(), err)
	}
	return env, nil
}

// Available GET / 健康检查。等价 python gateway_available（HTTP 200 视为可用）。
func (g *Gateway) Available(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	env, err := g.getJSON(ctx, "/", nil)
	return err == nil && env.Code == 0
}

// StockUniverse 网关全市场代码 → 600000.SH 形态列表（bj 跳过，解决次新股扩池）。
// 等价 python fetch_stock_universe。
func (g *Gateway) StockUniverse(ctx context.Context) ([]string, error) {
	env, err := g.getJSON(ctx, "/code/stocks", nil)
	if err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("gateway error: /code/stocks → %s", env.Msg)
	}
	var raw []string
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &raw); err != nil {
			return nil, fmt.Errorf("universe 解析失败: %w", err)
		}
	}
	universe := make([]string, 0, len(raw))
	for _, item := range raw {
		if sym, ok := FromGatewayItem(item); ok {
			universe = append(universe, sym)
		}
	}
	return universe, nil
}

// DailyKlines 拉单只股票最近 total 根不复权日线（Time 升序，旧在前）。
// 分页：单请求上限 800 根，start=0 为最新一页向历史翻页；页不足请求数即历史尽头。
// 等价 python fetch_daily。
func (g *Gateway) DailyKlines(ctx context.Context, gatewayCode string, total int) ([]Kline, error) {
	var pages [][]Kline
	for offset := 0; offset < total; {
		count := total - offset
		if count > PageLimit {
			count = PageLimit
		}
		q := url.Values{"code": {gatewayCode}, "start": {strconv.Itoa(offset)}, "count": {strconv.Itoa(count)}}
		env, err := g.getJSON(ctx, "/kline/day", q)
		if err != nil {
			return nil, err
		}
		if env.Code != 0 {
			return nil, fmt.Errorf("gateway error: /kline/day %s → %s", q.Encode(), env.Msg)
		}
		var data struct {
			List []Kline `json:"List"`
		}
		if len(env.Data) > 0 {
			if err := json.Unmarshal(env.Data, &data); err != nil {
				return nil, fmt.Errorf("kline 解析失败: %w", err)
			}
		}
		if len(data.List) == 0 {
			break
		}
		pages = append(pages, data.List)
		offset += len(data.List)
		if len(data.List) < count { // 页不足请求数 = 历史尽头
			break
		}
	}
	out := make([]Kline, 0, len(pages)*PageLimit)
	for i := len(pages) - 1; i >= 0; i-- { // 页序新→旧，页内旧→新：倒页序拼接后整体升序
		out = append(out, pages[i]...)
	}
	return out, nil
}

// DailyBars 拉取并换算为 Bar（等价 python fetch_daily + _parse_bars）。
func (g *Gateway) DailyBars(ctx context.Context, symbol string, total int) ([]Bar, error) {
	code, err := GatewayCode(symbol)
	if err != nil {
		return nil, err
	}
	kl, err := g.DailyKlines(ctx, code, total)
	if err != nil {
		return nil, err
	}
	bars := make([]Bar, 0, len(kl))
	for _, k := range kl {
		b, err := ToBar(symbol, k)
		if err != nil {
			return nil, err
		}
		bars = append(bars, b)
	}
	return bars, nil
}

// BatchResult 批量拉取结果：Bars 为全部成功 bar（按原 symbols 顺序拼接），
// Failed 为 "symbol: 原因" 列表（按原顺序）；OK 为成功只数
// （含拉到 0 根的票，与 python 一致）。
type BatchResult struct {
	Bars   []Bar
	OK     int
	Failed []string
}

// FetchBatch 并发批量拉取日线：单票失败重试 1 次后只收集不中断。
// workers ≤0 取默认 8。等价 python fetch_daily_batch（其无重试，按规格加 1 次）。
func FetchBatch(ctx context.Context, g *Gateway, symbols []string, total, workers int) *BatchResult {
	if workers <= 0 {
		workers = defaultWorkers
	}
	res := &BatchResult{}
	type job struct{ symbol, code string }
	var jobs []job
	for _, s := range symbols {
		code, err := GatewayCode(s)
		if err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", s, err))
			continue
		}
		jobs = append(jobs, job{symbol: s, code: code})
	}
	if len(jobs) == 0 {
		return res
	}

	barsBy := make([][]Bar, len(jobs))
	errs := make([]error, len(jobs))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			bars, err := fetchWithRetry(ctx, g, j.symbol, total)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[i] = err
				return
			}
			barsBy[i] = bars
		}(i, j)
	}
	wg.Wait()

	for i, j := range jobs {
		if errs[i] != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", j.symbol, errs[i]))
			continue
		}
		res.OK++
		res.Bars = append(res.Bars, barsBy[i]...)
	}
	return res
}

// fetchWithRetry 单票拉取，失败重试 1 次（共至多 2 次）。
func fetchWithRetry(ctx context.Context, g *Gateway, symbol string, total int) ([]Bar, error) {
	var bars []Bar
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		bars, err = g.DailyBars(ctx, symbol, total)
		if err == nil {
			return bars, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, err
}
