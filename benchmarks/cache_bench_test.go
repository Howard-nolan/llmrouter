//go:build bench

package benchmarks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type Corpus struct {
	Source    string    `json:"source"`
	Seed      int       `json:"seed"`
	Profile   string    `json:"profile"`
	NClusters int       `json:"n_clusters"`
	NPrompts  int       `json:"n_prompts"`
	Clusters  []Cluster `json:"clusters"`
}

type Cluster struct {
	ID      int      `json:"id"`
	Prompts []string `json:"prompts"`
}

type Result struct {
	Prompt       string
	ResponseText string
	CacheHit     bool
	Similarity   float64
	CostUSD      float64
	Provider     string
	Model        string
	Latency      time.Duration
	TTFT         time.Duration
}

type LogRecord struct {
	Prompt           string  `json:"prompt"`
	GatewayResponse  string  `json:"gateway_response"`
	BaselineResponse string  `json:"baseline_response"`
	CacheHit         bool    `json:"cache_hit"`
	Similarity       float64 `json:"similarity"`
	ModelRouted      string  `json:"model_routed"`
	CostUSD          float64 `json:"cost_usd"`
	TTFTMs           float64 `json:"ttft_ms"`
	LatencyMs        float64 `json:"latency_ms"`
}

var httpClient = &http.Client{Timeout: 120 * time.Second}

func TestCacheBenchmark(t *testing.T) {
	baseURL := getEnv("LLMROUTER_URL", "http://localhost:8080")
	corpusPath := getEnv("LLMROUTER_CORPUS", "data/corpus_realistic.json")
	model := getEnv("LLMROUTER_MODEL", "auto")
	concurrency := 3
	if v := os.Getenv("LLMROUTER_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			concurrency = n
		}
	}

	corpus, err := loadCorpus(corpusPath)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	fmt.Printf("Loaded %q: %d prompts across %d clusters\n",
		corpus.Profile, corpus.NPrompts, corpus.NClusters)

	prompts := flattenAndShuffle(corpus)

	// Logging mode: when LLMROUTER_LOG_PATH is set, write one JSONL
	// record per request. On every HIT, issue a second X-Cache: skip
	// call to capture a fresh baseline_response (the cached response
	// was generated for a different prompt — baseline shows fresh
	// quality for THIS prompt). When LLMROUTER_BASELINE_MODEL is also
	// set, additionally baseline on misses whose routed model differs
	// from the baseline model — used for the realistic-corpus bench
	// where we want to evaluate cheap-routed responses against the
	// quality model. Quality-routed misses (routed model == baseline
	// model) skip the baseline since it would be same-model noise.
	logPath := os.Getenv("LLMROUTER_LOG_PATH")
	baselineModel := os.Getenv("LLMROUTER_BASELINE_MODEL")
	var logFile *os.File
	if logPath != "" {
		logFile, err = os.Create(logPath)
		if err != nil {
			t.Fatalf("create log file: %v", err)
		}
		defer logFile.Close()
		extra := ""
		if baselineModel != "" {
			extra = fmt.Sprintf(", baseline-model=%s on cheap-routed misses", baselineModel)
		}
		fmt.Printf("Log: %s (baseline on HITs%s)\n", logPath, extra)
	}

	fmt.Println("Flushing cache...")
	if err := flushCache(baseURL); err != nil {
		t.Fatalf("flush cache: %v", err)
	}

	savedBefore, err := scrapeCostSaved(baseURL)
	if err != nil {
		t.Fatalf("scrape metrics (before): %v", err)
	}

	fmt.Printf("Running %d prompts against %s (model=%s, concurrency=%d)\n",
		len(prompts), baseURL, model, concurrency)
	results := make([]Result, 0, len(prompts))
	runStart := time.Now()

	type outcome struct {
		res          Result
		baselineText string
		err          error
		promptIdx    int
	}
	jobs := make(chan int, len(prompts))
	out := make(chan outcome, len(prompts))

	for w := 0; w < concurrency; w++ {
		go func() {
			for i := range jobs {
				res, err := sendRequest(baseURL, model, prompts[i], false)
				if err != nil {
					out <- outcome{err: err, promptIdx: i}
					continue
				}
				var baseline string
				if logFile != nil {
					// Baseline triggers on HIT (cached response was for a
					// different prompt) OR on a miss whose routed model
					// differs from the configured baseline model.
					needBaseline := res.CacheHit ||
						(baselineModel != "" && res.Model != baselineModel)
					if needBaseline {
						bm := baselineModel
						if bm == "" {
							bm = model
						}
						bres, berr := sendRequest(baseURL, bm, prompts[i], true)
						if berr != nil {
							out <- outcome{err: fmt.Errorf("baseline call: %w", berr), promptIdx: i}
							continue
						}
						baseline = bres.ResponseText
					}
				}
				out <- outcome{res: res, baselineText: baseline, promptIdx: i}
			}
		}()
	}
	for i := range prompts {
		jobs <- i
	}
	close(jobs)

	var logEncoder *json.Encoder
	if logFile != nil {
		logEncoder = json.NewEncoder(logFile)
	}

	for i := 0; i < len(prompts); i++ {
		o := <-out
		if o.err != nil {
			t.Fatalf("request %d (%q): %v", o.promptIdx, truncate(prompts[o.promptIdx], 60), o.err)
		}
		results = append(results, o.res)
		if logEncoder != nil {
			rec := LogRecord{
				Prompt:           o.res.Prompt,
				GatewayResponse:  o.res.ResponseText,
				BaselineResponse: o.baselineText,
				CacheHit:         o.res.CacheHit,
				Similarity:       o.res.Similarity,
				ModelRouted:      o.res.Model,
				CostUSD:          o.res.CostUSD,
				TTFTMs:           float64(o.res.TTFT.Microseconds()) / 1000.0,
				LatencyMs:        float64(o.res.Latency.Microseconds()) / 1000.0,
			}
			if err := logEncoder.Encode(rec); err != nil {
				t.Fatalf("write log record: %v", err)
			}
		}
		done := i + 1
		if done%25 == 0 || done == len(prompts) {
			fmt.Printf("  %d/%d (elapsed %s)\n", done, len(prompts), time.Since(runStart).Round(time.Second))
		}
	}

	savedAfter, err := scrapeCostSaved(baseURL)
	if err != nil {
		t.Fatalf("scrape metrics (after): %v", err)
	}

	summarize(results, savedAfter-savedBefore, time.Since(runStart))
}

func loadCorpus(path string) (*Corpus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Corpus
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func flattenAndShuffle(c *Corpus) []string {
	out := make([]string, 0, c.NPrompts)
	for _, cl := range c.Clusters {
		out = append(out, cl.Prompts...)
	}
	seed := uint64(c.Seed)
	rng := rand.New(rand.NewPCG(seed, seed))
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func flushCache(baseURL string) error {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/cache/flush", nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func sendRequest(baseURL, model, prompt string, skipCache bool) (Result, error) {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"stream":     true,
		"max_tokens": 512,
	})
	if err != nil {
		return Result{}, err
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if skipCache {
		req.Header.Set("X-Cache", "skip")
	}

	start := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return Result{}, fmt.Errorf("status %d: %s", resp.StatusCode, errBody)
	}

	// Parse SSE stream: each event is "data: {json}\n\n", terminated by
	// "data: [DONE]". Concatenate delta.content across chunks for the
	// assistant message text; cost_usd appears only on the final chunk.
	// TTFT = wall time from request send to first data event.
	var costUSD float64
	var text strings.Builder
	var firstDataAt time.Time
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if firstDataAt.IsZero() {
			firstDataAt = time.Now()
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			CostUSD *float64 `json:"cost_usd"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
		if chunk.CostUSD != nil {
			costUSD = *chunk.CostUSD
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, fmt.Errorf("scan SSE: %w", err)
	}
	latency := time.Since(start)
	var ttft time.Duration
	if !firstDataAt.IsZero() {
		ttft = firstDataAt.Sub(start)
	}

	res := Result{
		Prompt:       prompt,
		ResponseText: text.String(),
		Provider:     resp.Header.Get("X-LLMRouter-Provider"),
		Model:        resp.Header.Get("X-LLMRouter-Model"),
		CacheHit:     resp.Header.Get("X-LLMRouter-Cache") == "HIT",
		CostUSD:      costUSD,
		Latency:      latency,
		TTFT:         ttft,
	}
	if v := resp.Header.Get("X-LLMRouter-Similarity"); v != "" {
		res.Similarity, _ = strconv.ParseFloat(v, 64)
	}
	return res, nil
}

// scrapeCostSaved sums the llmrouter_cost_saved_by_cache_usd_total counter
// across all label combinations. Parses Prometheus text exposition format.
func scrapeCostSaved(baseURL string) (float64, error) {
	resp, err := httpClient.Get(baseURL + "/metrics")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	const metric = "llmrouter_cost_saved_by_cache_usd_total"
	var total float64
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, metric) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		total += v
	}
	return total, scanner.Err()
}

func summarize(results []Result, costSaved float64, wallTime time.Duration) {
	var hits, misses int
	var actualCost float64
	var hitLat, missLat, hitTTFT, missTTFT []time.Duration
	missByModel := map[string]int{}
	missCostByModel := map[string]float64{}

	for _, r := range results {
		if r.CacheHit {
			hits++
			hitLat = append(hitLat, r.Latency)
			hitTTFT = append(hitTTFT, r.TTFT)
		} else {
			misses++
			missLat = append(missLat, r.Latency)
			missTTFT = append(missTTFT, r.TTFT)
			actualCost += r.CostUSD
			missByModel[r.Model]++
			missCostByModel[r.Model] += r.CostUSD
		}
	}

	total := len(results)
	hitRate := 0.0
	if total > 0 {
		hitRate = float64(hits) / float64(total) * 100
	}
	savingsRate := 0.0
	if actualCost+costSaved > 0 {
		savingsRate = costSaved / (actualCost + costSaved) * 100
	}

	fmt.Println()
	fmt.Println("=== Summary ===")
	fmt.Printf("Wall time:      %s\n", wallTime.Round(time.Millisecond))
	fmt.Printf("Requests:       %d (%d hits, %d misses)\n", total, hits, misses)
	fmt.Printf("Hit rate:       %.1f%%\n", hitRate)
	fmt.Printf("Actual cost:    $%.4f\n", actualCost)
	fmt.Printf("Cost saved:     $%.4f\n", costSaved)
	fmt.Printf("Savings rate:   %.1f%%\n", savingsRate)
	fmt.Printf("Hit latency   p50/p95/p99: %s / %s / %s\n",
		pct(hitLat, 50), pct(hitLat, 95), pct(hitLat, 99))
	fmt.Printf("Miss latency  p50/p95/p99: %s / %s / %s\n",
		pct(missLat, 50), pct(missLat, 95), pct(missLat, 99))
	fmt.Printf("Hit TTFT      p50/p95/p99: %s / %s / %s\n",
		pct(hitTTFT, 50), pct(hitTTFT, 95), pct(hitTTFT, 99))
	fmt.Printf("Miss TTFT     p50/p95/p99: %s / %s / %s\n",
		pct(missTTFT, 50), pct(missTTFT, 95), pct(missTTFT, 99))
	if len(missByModel) > 0 {
		models := make([]string, 0, len(missByModel))
		for m := range missByModel {
			models = append(models, m)
		}
		sort.Strings(models)
		fmt.Println("Misses by routed model:")
		for _, m := range models {
			fmt.Printf("  %-40s %d ($%.4f)\n", m, missByModel[m], missCostByModel[m])
		}
	}
}

func pct(xs []time.Duration, p int) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(xs))
	copy(sorted, xs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := p * len(sorted) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
