package main

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/go-redis/redis/v8"
)

var (
	redisClient     *redis.Client
	currentProxyURL string
	headerRules     []HeaderRule
	logFile         *os.File
	requestLogFile  *os.File
	enableDetection bool
)

func configureLogger(verbose bool) {
	var err error
	logFile, err = os.OpenFile("proxy.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}

	if verbose {
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	} else {
		log.SetOutput(logFile)
	}

	requestLogFile, err = os.OpenFile("requests.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Failed to open request log file: %v", err)
	}
}

func logInfo(format string, v ...interface{}) {

	log.Printf(ColorBlue+"INFO: "+format+ColorReset, v...)
}

func logError(format string, v ...interface{}) {
	log.Printf(ColorRed+"ERROR: "+format+ColorReset, v...)
}

func logWarning(format string, v ...interface{}) {
	log.Printf(ColorYellow+"WARNING: "+format+ColorReset, v...)
}

func logSuccess(format string, v ...interface{}) {
	log.Printf(ColorGreen+"SUCCESS: "+format+ColorReset, v...)
}

func logRequest(r *http.Request) {
	log.SetOutput(requestLogFile)
	log.Printf("Request: %s %s from %s", r.Method, r.URL.String(), r.RemoteAddr)
	log.SetOutput(logFile)
}

const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
)

func init() {
	var err error
	logFile, err = os.OpenFile("proxy.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}
	log.SetOutput(logFile)

	requestLogFile, err = os.OpenFile("requests.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Failed to open request log file: %v", err)
	}
}

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()

		gzw := gzipResponseWriter{ResponseWriter: w, Writer: gz}
		next.ServeHTTP(gzw, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	Writer *gzip.Writer
}

func (gzw gzipResponseWriter) Write(data []byte) (int, error) {
	return gzw.Writer.Write(data)
}

func loadHeaderRules(filename string) []HeaderRule {
	data, err := os.ReadFile(filename)
	if err != nil {
		logError("Failed to read header rules file: " + err.Error())
	}

	var config HeaderRulesConfig
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		logError("Failed to parse header rules: " + err.Error())
	}
	logSuccess("Header rules loaded successfully")
	return config.HeaderRules
}

func applyHeaderRules(resp *http.Response) {
	for _, rule := range headerRules {
		switch rule.Action {
		case "add-header":
			resp.Header.Add(rule.Header, rule.Value)
		case "set-header":
			resp.Header.Set(rule.Header, rule.Value)
		case "del-header":
			resp.Header.Del(rule.Header)
		case "replace-header":
			if rule.Regex != "" {
				if value := resp.Header.Get(rule.Header); value != "" {
					re := regexp.MustCompile(rule.Regex)
					newValue := re.ReplaceAllString(value, rule.Replacement)
					resp.Header.Set(rule.Header, newValue)
				}
			}
		}
	}
}

func getServerIPAddress() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		logError("Failed to get network interfaces: " + err.Error())
	}
	var loopbackIP string
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok {
			if ipNet.IP.To4() != nil {
				if ipNet.IP.IsLoopback() {
					loopbackIP = ipNet.IP.String()
				} else {
					return ipNet.IP.String()
				}
			}
		}
	}
	if loopbackIP != "" {
		return loopbackIP
	}
	log.Fatalf("No valid IP address found")
	return ""
}

// StartProxyServer starts a proxy server on the given address and forwards requests to the backendURL.
func StartProxyServer(proxyID, address, backendURL string, queue *Queue, enableDetection bool) {
	parsedURL, err := url.Parse(backendURL)
	if err != nil {
		log.Fatalf("Failed to parse backend URL: %v", err)
	}

	redisClient = redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})

	proxy := httputil.NewSingleHostReverseProxy(parsedURL)
	originalDirector := proxy.Director
	if queue == nil {
		proxy.Director = originalDirector
	} else {
		proxy.Director = func(req *http.Request) {
			originalDirector(req)
			req.Header.Add("X-Proxy-ID", proxyID)

			messages, err := queue.client.XReadGroup(ctx, &redis.XReadGroupArgs{
				Group:    "proxy_group",
				Consumer: "proxy_consumer",
				Streams:  []string{"proxy_requests", ">"},
				Count:    1,
				Block:    10 * time.Millisecond,
			}).Result()

			if err != nil {
				return
			}

			for _, msg := range messages[0].Messages {
				if msg.Values["block"] == "true" {
					req.URL.Host = ""
					logInfo("Blocked request based on queue message")
				}
				if newURL, ok := msg.Values["redirect_url"].(string); ok {
					parsedNewURL, _ := url.Parse(newURL)
					req.URL.Scheme = parsedNewURL.Scheme
					req.URL.Host = parsedNewURL.Host
					req.URL.Path = parsedNewURL.Path
					logInfo("Redirected request to new URL: %s", newURL)
				}
			}
		}
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		applyHeaderRules(resp)

		if strings.Contains(resp.Request.Header.Get("Accept-Encoding"), "gzip") {
			logInfo("Applying Gzip compression to response for URL: %s", resp.Request.URL)

			resp.Header.Set("Content-Encoding", "gzip")
			resp.Header.Del("Content-Length")

			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			_, err := io.Copy(gz, resp.Body)
			if err != nil {
				resp.Body.Close()
				logError("Error during Gzip compression: %v", err)
				return err
			}
			gz.Close()

			resp.Body.Close()
			resp.Body = io.NopCloser(&buf)
			resp.ContentLength = -1

			logSuccess("Gzip compression applied successfully for URL: %s", resp.Request.URL)
		}

		return nil
	}

	var requestCounts = make(map[string]int)
	var mu sync.Mutex
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Proxy is healthy"))
	})

	handlerWithMiddleware := gzipMiddleware(mux)
	activeProxy, err := redisClient.Get(ctx, "active_proxy").Result()
	if err != nil {
		logError("Failed to get active proxy: %v", err)
		activeProxy = "https://localhost"
	}
	currentProxyURL = activeProxy

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		status := "200"

		defer func() {
			duration := time.Since(start).Seconds()
			proxyRequestsTotal.WithLabelValues(proxyID, status, r.Method).Inc()
			proxyRequestDuration.WithLabelValues(proxyID, r.Method).Observe(duration)
		}()
		logRequest(r)
		mu.Lock()
		defer mu.Unlock()

		ip := r.RemoteAddr
		requestCounts[ip]++

		if requestCounts[ip] > 500 {
			status = "429"
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}

		logInfo("%s received : %s", proxyID, r.URL.String())

		activeProxy, err := redisClient.Get(ctx, "active_proxy").Result()
		if err != nil {
			logError("Failed to get active proxy: %v", err)
			status = "500"
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		if "https://"+r.Host != activeProxy {
			status = "302"
			http.Redirect(w, r, activeProxy+r.RequestURI, http.StatusFound)
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			status = "500"
			http.Error(w, "Failed to read request body", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		if enableDetection {
			// Appel au service de détection si activé
			detectionData := map[string]interface{}{
				"uri":  r.RequestURI,
				"body": string(bodyBytes),
			}
			detectionResponse, err := sendToDetectionService(detectionData)
			if err != nil {
				status = "500"
				http.Error(w, "Failed to connect to detection service", http.StatusInternalServerError)
				return
			}

			if detectionResponse["authorized"] == "MALICIOUS" {
				status = "403"
				htmlContent, err := os.ReadFile("403.html")
				if err != nil {
					status = "500"
					http.Error(w, "Failed to load 403 page", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusForbidden)
				w.Write(htmlContent)
				return
			}
		}

		proxy.ServeHTTP(w, r)
	})

	go func() {
		pubsub := redisClient.Subscribe(ctx, "proxy_updates")
		defer pubsub.Close()

		for msg := range pubsub.Channel() {
			logInfo("Received new proxy update: %s", msg.Payload)
			currentProxyURL = msg.Payload
		}
	}()

	server := &http.Server{
		Addr:    "0.0.0.0" + address,
		Handler: handlerWithMiddleware,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	logInfo("Starting HTTPS proxy server %s on %s", proxyID, address)
	server.ListenAndServeTLS("server.crt", "server.key")
}
