package syncdaily

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// unmarshalKlines 从网关响应 JSON 中取 List（测试辅助）。
func unmarshalKlines(envJSON string) ([]Kline, error) {
	var env struct {
		Code int `json:"code"`
		Data struct {
			List []Kline `json:"List"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(envJSON), &env); err != nil {
		return nil, err
	}
	return env.Data.List, nil
}

// recServer 记录请求的 mock 网关。
type recServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string // r.URL.RawQuery 列表
}

func (s *recServer) queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

// newRecServer 起 mock 网关；handler 每请求先记录 RawQuery。
func newRecServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, rawQuery string)) *recServer {
	t.Helper()
	rec := &recServer{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r.URL.RawQuery)
		rec.mu.Unlock()
		handle(w, r, r.URL.RawQuery)
	}))
	t.Cleanup(rec.Close)
	return rec
}

// writeEnv 输出统一响应。
func writeEnv(w http.ResponseWriter, code int, msg string, data string) {
	_ = json.NewEncoder(w).Encode(json.RawMessage(fmt.Sprintf(
		`{"code":%d,"msg":%q,"data":%s}`, code, msg, data)))
}

// klineAt 生成第 i 个交易日的 bar（i 越大越新）。
func klineAt(i int) Kline {
	tm := time.Date(2020, 1, 1, 15, 0, 0, 0, time.Local).AddDate(0, 0, i)
	return Kline{Open: int64(1000 + i), High: 1100, Low: 900, Close: int64(1050 + i),
		Volume: int64(1000 + i), Amount: int64(100000 + i), Time: tm.Format("2006-01-02T15:00:00+08:00")}
}

// serveHistoryPage 按 tdx 网关语义切页：start=0 为最新一页（页内 Time 升序），
// 历史共 n 根（索引 0 最旧）。
func serveHistoryPage(w http.ResponseWriter, rawQuery string, n int) {
	q := map[string]string{}
	for _, kv := range strings.Split(rawQuery, "&") {
		p := strings.SplitN(kv, "=", 2)
		if len(p) == 2 {
			q[p[0]] = p[1]
		}
	}
	start, _ := strconv.Atoi(q["start"])
	count, _ := strconv.Atoi(q["count"])
	lo := n - start - count
	if lo < 0 {
		lo = 0
	}
	hi := n - start // 不含
	if hi > n {
		hi = n
	}
	if hi < lo {
		hi = lo
	}
	var items []Kline
	for i := lo; i < hi; i++ {
		items = append(items, klineAt(i))
	}
	buf, _ := json.Marshal(items)
	writeEnv(w, 0, "ok", fmt.Sprintf(`{"Count":%d,"List":%s}`, len(items), buf))
}

// TestGatewayAvailable 健康检查：200 可用 / 拒连不可用。
func TestGatewayAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnv(w, 0, "ok", `{"status":"running"}`)
	}))
	defer srv.Close()
	gw := NewGateway(srv.URL)
	if !gw.Available(context.Background()) {
		t.Error("健康检查应通过")
	}
	srv.Close()
	if gw.Available(context.Background()) {
		t.Error("网关关闭后健康检查应失败")
	}
}

// TestStockUniverse 全市场代码 → 带后缀形态，bj 跳过（真实响应 fixture）。
func TestStockUniverse(t *testing.T) {
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, rawQuery string) {
		if !strings.HasPrefix(r.URL.Path, "/code/stocks") {
			writeEnv(w, 1, "404", "null")
			return
		}
		writeEnv(w, 0, "ok", `["sh600000","sz000001","bj830001","sh688166","sz300750"]`)
	})
	got, err := NewGateway(srv.URL).StockUniverse(context.Background())
	if err != nil {
		t.Fatalf("StockUniverse: %v", err)
	}
	want := []string{"600000.SH", "000001.SZ", "688166.SH", "300750.SZ"}
	if len(got) != len(want) {
		t.Fatalf("universe = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("universe[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestStockUniverseError 业务码非 0 报错。
func TestStockUniverseError(t *testing.T) {
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeEnv(w, 1, "无法识别的代码", "null")
	})
	if _, err := NewGateway(srv.URL).StockUniverse(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "无法识别的代码") {
		t.Errorf("code=1 应报错含 msg, got %v", err)
	}
}

// TestDailyKlinesRealFixture 单页真实响应：字段名大小写、逐位换算入口。
func TestDailyKlinesRealFixture(t *testing.T) {
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, rawQuery string) {
		if !strings.Contains(rawQuery, "code=600000") {
			writeEnv(w, 1, "无法识别的代码", "null")
			return
		}
		_, _ = w.Write([]byte(realKlineJSON))
	})
	kl, err := NewGateway(srv.URL).DailyKlines(context.Background(), "600000", 40)
	if err != nil {
		t.Fatalf("DailyKlines: %v", err)
	}
	if len(kl) != 2 || kl[0].Open != 9020 || kl[0].Amount != 1386209920000 || kl[1].Close != 9540 {
		t.Errorf("kl = %+v", kl)
	}
	if kl[0].Time != "2026-09-30T15:00:00+08:00" {
		t.Errorf("Time = %q", kl[0].Time)
	}
}

// TestDailyKlinesPagination >800 根分两次拉拼装；页不足请求数即停；
// 请求 count 不超过 800；结果整体 Time 升序（旧在前）。
func TestDailyKlinesPagination(t *testing.T) {
	const n = 950 // 历史共 950 根
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, rawQuery string) {
		serveHistoryPage(w, rawQuery, n)
	})
	kl, err := NewGateway(srv.URL).DailyKlines(context.Background(), "600000", 1000)
	if err != nil {
		t.Fatalf("DailyKlines: %v", err)
	}
	// 请求序列：(start=0,count=800) → (start=800,count=200)；第二页只有 150 根 < 200 即停
	queries := srv.queries()
	if len(queries) != 2 {
		t.Fatalf("预期 2 次请求, got %d: %v", len(queries), queries)
	}
	if queries[0] != "code=600000&count=800&start=0" || queries[1] != "code=600000&count=200&start=800" {
		t.Errorf("请求参数 = %v", queries)
	}
	if len(kl) != n {
		t.Fatalf("预期拼装 %d 根, got %d", n, len(kl))
	}
	for i, k := range kl { // 整体升序：第 i 根 = 历史索引 i
		if want := klineAt(i).Time; k.Time != want {
			t.Fatalf("kl[%d].Time = %q, want %q", i, k.Time, want)
		}
	}
}

// TestDailyKlinesShortPage 单页即历史尽头：页不足请求数提前停止。
func TestDailyKlinesShortPage(t *testing.T) {
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, rawQuery string) {
		serveHistoryPage(w, rawQuery, 120) // 历史只有 120 根 < 请求 800
	})
	kl, err := NewGateway(srv.URL).DailyKlines(context.Background(), "600000", 800)
	if err != nil {
		t.Fatalf("DailyKlines: %v", err)
	}
	if len(kl) != 120 {
		t.Errorf("预期 120 根, got %d", len(kl))
	}
	if got := len(srv.queries()); got != 1 {
		t.Errorf("预期 1 次请求, got %d", got)
	}
}

// TestDailyKlinesEmpty 空页（data.List 空返回，如无数据票）→ 空结果、单请求。
func TestDailyKlinesEmpty(t *testing.T) {
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeEnv(w, 0, "ok", `{"Count":0,"List":[]}`)
	})
	kl, err := NewGateway(srv.URL).DailyKlines(context.Background(), "600000", 40)
	if err != nil {
		t.Fatalf("DailyKlines: %v", err)
	}
	if len(kl) != 0 || len(srv.queries()) != 1 {
		t.Errorf("kl=%d, reqs=%d; want 0,1", len(kl), len(srv.queries()))
	}
}

// TestDailyKlinesGatewayError 业务码非 0（如超 800 上限）报错。
func TestDailyKlinesGatewayError(t *testing.T) {
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeEnv(w, 1, "单次数量不能超过800", "null")
	})
	if _, err := NewGateway(srv.URL).DailyKlines(context.Background(), "600000", 900); err == nil ||
		!strings.Contains(err.Error(), "单次数量不能超过800") {
		t.Errorf("应报错含网关 msg, got %v", err)
	}
}

// TestDailyKlinesHTTPError HTTP 非 200 报错。
func TestDailyKlinesHTTPError(t *testing.T) {
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.WriteHeader(http.StatusInternalServerError)
		writeEnv(w, 1, "boom", "null")
	})
	if _, err := NewGateway(srv.URL).DailyKlines(context.Background(), "600000", 40); err == nil ||
		!strings.Contains(err.Error(), "gateway HTTP 500") {
		t.Errorf("应报错 gateway HTTP 500, got %v", err)
	}
}

// TestDailyBars 拉取+换算整链（符号→裸代码→分页→ToBar）。
func TestDailyBars(t *testing.T) {
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, rawQuery string) {
		_, _ = w.Write([]byte(realKlineJSON))
	})
	bars, err := NewGateway(srv.URL).DailyBars(context.Background(), "600000.SH", 40)
	if err != nil {
		t.Fatalf("DailyBars: %v", err)
	}
	if len(bars) != 2 || bars[0].Symbol != "600000.SH" || bars[0].Market != "CN.SH" ||
		bars[0].Close.String() != "9.02" || bars[0].Volume != 147484800 {
		t.Errorf("bars = %+v", bars)
	}
}

// TestFetchBatchFailCollect 单票失败只收集不中断；非法代码前置拒绝；
// 结果按原 symbols 顺序。
func TestFetchBatchFailCollect(t *testing.T) {
	var hits struct {
		mu     sync.Mutex
		sh0001 int
	}
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, rawQuery string) {
		switch {
		case strings.Contains(rawQuery, "code=600000"):
			_, _ = w.Write([]byte(realKlineJSON))
		case strings.Contains(rawQuery, "code=000001"):
			hits.mu.Lock()
			hits.sh0001++
			hits.mu.Unlock()
			writeEnv(w, 1, "gateway boom", "null")
		default:
			writeEnv(w, 1, "unknown", "null")
		}
	})
	res := FetchBatch(context.Background(), NewGateway(srv.URL),
		[]string{"600000.SH", "000001.SZ", "830001.BJ", "badcode"}, 40, 8)

	if res.OK != 1 {
		t.Errorf("OK = %d, want 1", res.OK)
	}
	if len(res.Bars) != 2 || res.Bars[0].Symbol != "600000.SH" {
		t.Errorf("Bars = %+v", res.Bars)
	}
	hits.mu.Lock()
	defer hits.mu.Unlock()
	if hits.sh0001 != 2 {
		t.Errorf("失败票应重试 1 次（共 2 次请求）, got %d", hits.sh0001)
	}
	joined := strings.Join(res.Failed, "\n")
	for _, want := range []string{"000001.SZ: ", "830001.BJ: BJ not supported", "badcode: symbol must have exchange suffix"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Failed 缺 %q:\n%s", want, joined)
		}
	}
	if len(res.Failed) != 3 {
		t.Errorf("Failed = %v", res.Failed)
	}
}

// TestFetchBatchRetryThenSuccess 首次失败重试成功。
func TestFetchBatchRetryThenSuccess(t *testing.T) {
	var hits struct {
		mu sync.Mutex
		n  int
	}
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		hits.mu.Lock()
		hits.n++
		first := hits.n == 1
		hits.mu.Unlock()
		if first {
			w.WriteHeader(http.StatusInternalServerError)
			writeEnv(w, 1, "boom", "null")
			return
		}
		_, _ = w.Write([]byte(realKlineJSON))
	})
	res := FetchBatch(context.Background(), NewGateway(srv.URL), []string{"600000.SH"}, 40, 4)
	if res.OK != 1 || len(res.Bars) != 2 || len(res.Failed) != 0 {
		t.Errorf("重试应成功: OK=%d bars=%d failed=%v", res.OK, len(res.Bars), res.Failed)
	}
	hits.mu.Lock()
	defer hits.mu.Unlock()
	if hits.n != 2 {
		t.Errorf("预期 2 次请求, got %d", hits.n)
	}
}

// TestFetchBatchAllInvalid 全部非法 → 空结果不发起请求。
func TestFetchBatchAllInvalid(t *testing.T) {
	srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeEnv(w, 0, "ok", `[]`)
	})
	res := FetchBatch(context.Background(), NewGateway(srv.URL), []string{"badcode"}, 40, 4)
	if res.OK != 0 || len(res.Bars) != 0 || len(res.Failed) != 1 {
		t.Errorf("res = %+v", res)
	}
	if got := len(srv.queries()); got != 0 {
		t.Errorf("不应发起请求, got %d", got)
	}
}
