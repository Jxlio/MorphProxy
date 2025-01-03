package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var ctx = context.Background()
var unsecureCert bool
var serverIP string
var domain string
var proxyManager *ProxyManager
var aclConfig *ACLConfig

// Metrics for Prometheus monitoring
var (
	proxyRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_requests_total",
			Help: "Total number of requests handled by each proxy",
		},
		[]string{"proxy_id", "status", "method"},
	)
	proxyRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "proxy_request_duration_seconds",
			Help:    "Histogram of request durations per proxy",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"proxy_id", "method"},
	)
)
var proxySwitchesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "proxy_switches_total",
		Help: "Total number of proxy switches",
	},
	[]string{"proxy_id"},
)

func checkCertificates(certFile, keyFile string) error {
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		return fmt.Errorf("certificate file %s does not exist", certFile)
	}
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		return fmt.Errorf("key file %s does not exist", keyFile)
	}
	return nil
}

func init() {
	prometheus.MustRegister(proxyRequestsTotal, proxyRequestDuration)
	prometheus.MustRegister(proxySwitchesTotal)
}

func generateAPIKey() string {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	if err != nil {
		log.Fatalf("Failed to generate API key: %v", err)
	}
	return hex.EncodeToString(key)
}

func NewProxyManager(proxyURLs []string, domain string) *ProxyManager {
	logInfo("Initializing ProxyManager with domain: %s", domain)
	proxies := make([]*url.URL, len(proxyURLs))
	for i, proxyURL := range proxyURLs {
		url, err := url.Parse(proxyURL)
		if err != nil {
			logError("Invalid proxy URL: %s", proxyURL)
		}
		proxies[i] = url
	}

	pm := &ProxyManager{
		proxies:      proxies,
		currentProxy: proxies[0],
		ticker:       time.NewTicker(10 * time.Second),
		domain:       domain, // Initialisez le champ
	}

	go pm.startAutoSwitch()

	return pm
}

// ServeHTTP dynamically proxies the request through one of the managed proxies
func (pm *ProxyManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := "200"

	defer func() {
		duration := time.Since(start).Seconds()
		proxyID := pm.GetProxy().String()

		proxyRequestsTotal.WithLabelValues(proxyID, status, r.Method).Inc()
		proxyRequestDuration.WithLabelValues(proxyID, r.Method).Observe(duration)
	}()

	activeProxy, err := pm.GetActiveProxy()
	if err != nil {
		status = "500"
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	requestedURL := r.URL
	activeProxyURL := pm.GetProxy()

	if requestedURL.Scheme != activeProxyURL.Scheme || strings.Split(requestedURL.Host, ":")[0] != strings.Split(activeProxyURL.Host, ":")[0] {
		http.Redirect(w, r, activeProxyURL.String()+r.RequestURI, http.StatusTemporaryRedirect)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(activeProxy)
	EnableSkipSecureVerify(proxy)

	proxy.ServeHTTP(w, r)
}

func main() {
	ServerIPArg := flag.String("ip", "", "Define the Public IP address of the proxies")
	headerRulesFile := flag.String("header-rules", "", "Path to the header rules YAML file")
	verbose := flag.Bool("v", false, "Enable verbose logging to the terminal")
	queueSystem := flag.Bool("queue-system", false, "Queue system to use (redis or kafka)")
	enableDetection := flag.Bool("enable-detection", false, "Enable or disable the attack detection system")
	unsecureCertVerification := flag.Bool("unsecure-cert", false, "Enable skipping unsecure certifate verification")
	proxyCount := flag.Int("proxy-count", 4, "Number of proxies to deploy in rotation")
	proxyPorts := flag.String("proxy-ports", "8081,8082,8083,8084", "Comma-separated list of ports for proxies")
	BackendURLFlag := flag.String("web-server", "http://127.0.0.1:5000", "Define the backend web server URL")
	apiFlag := flag.Bool("api", false, "Define the API endpoint")
	aclFile := flag.String("acl-file", "", "Path to the YAML file defining ACLs")
	domain := flag.String("d", "", "Domain name to use for the proxy (e.g., jxlio.fr)")
	certFile := flag.String("crt", "", "Path to the SSL certificate file")
	keyFile := flag.String("key", "", "Path to the SSL key file")
	flag.Parse()

	var serverIP string
	configureLogger(*verbose)

	if err := checkCertificates("server.crt", "server.key"); err != nil {
		log.Fatalf("Certificate check failed: %v", err)
	}

	if *ServerIPArg != "" {
		if *domain != "" {
			serverIP = *domain
			logInfo("Using %s as the host", serverIP)
		} else {
			logInfo("IP address %s has been manually set", *ServerIPArg)
			serverIP = *ServerIPArg
		}
	} else {
		if *domain != "" {
			serverIP = *domain
			logInfo("Using %s as the host", serverIP)
		} else {
			serverIP = getServerIPAddress()
			logWarning("No IP address specified. Using %s as the server IP address", serverIP)
		}
	}

	if *headerRulesFile != "" {
		logInfo("Loading header rules from %s", *headerRulesFile)
		headerRules = loadHeaderRules(*headerRulesFile)
	} else {
		logWarning("No header rules specified. Header modification is disabled.")
	}

	if *queueSystem {
		queue := NewQueue("localhost:6379", "proxy_requests", "proxy_group")
		if err := ensureQueueSetup(queue); err != nil {
			logError("Failed to setup queue: %v", err)
		}
		addTestMessage(queue)
		go startConsumers(queue)
	}

	if *aclFile != "" {
		var err error
		aclConfig, err = LoadACLConfig(*aclFile)
		if err != nil {
			log.Fatalf("Failed to load ACL file: %v", err)
		}
	}
	if *BackendURLFlag != "" {
		backendURLserver = *BackendURLFlag
	} else {
		backendURLserver = "http://127.0.0.1:5000"
	}
	if *unsecureCertVerification {
		unsecureCert = true
		logWarning("Secure certificate verification is disabled.")
	} else {
		unsecureCert = false
	}

	portList := strings.Split(*proxyPorts, ",")
	if len(portList) < *proxyCount {
		logWarning("Insufficient ports provided. Using default ports.")
		portList = []string{"8081", "8082", "8083", "8084"}
	}

	if *domain == "" || *certFile == "" || *keyFile == "" {
		logWarning("Domain (-d), certificate (-crt), and key (-key) are required to setup custom domain, using local certificate instead")
	}
	// Proxy configurations
	proxyConfigs := []struct {
		id         string
		address    string
		backendURL string
	}{}
	for i := 0; i < *proxyCount; i++ {
		port := portList[i%len(portList)]
		proxyConfigs = append(proxyConfigs, struct {
			id         string
			address    string
			backendURL string
		}{
			id:         fmt.Sprintf("proxy%d", i+1),
			address:    ":" + port,
			backendURL: backendURLserver,
		})
	}

	proxyURLs := []string{}

	for _, config := range proxyConfigs {
		proxyURLs = append(proxyURLs, "https://"+serverIP+config.address)
	}

	proxyManager := NewProxyManager(proxyURLs, *domain)

	mux := http.NewServeMux()
	if *apiFlag {
		logInfo("API endpoint enabled")
		//generate an random api key
		apiKey := generateAPIKey()
		logInfo("API Key: %s", apiKey)
		setupAPIRoutes(mux, proxyManager, apiKey)
	}

	if *queueSystem {
		queue := NewQueue("localhost:6379", "proxy_requests", "proxy_group")
		for _, config := range proxyConfigs {
			go StartProxyServer(config.id, config.address, config.backendURL, queue, *enableDetection, proxyManager)
		}
	} else {
		for _, config := range proxyConfigs {
			go StartProxyServer(config.id, config.address, config.backendURL, nil, *enableDetection, proxyManager)
		}
	}

	var suspiciousRating *SuspiciousRating
	if *enableDetection {
		logInfo("Attack detection system enabled")
		suspiciousRating = NewSuspiciousRating("localhost:6379", 20)
	} else {
		logInfo("Attack detection system disabled")
	}

	// Setup Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.Handler())

	mux.Handle("/", SessionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if aclConfig != nil {
			if handled := HandleRequestWithACL(r, w, aclConfig); handled {
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if *apiFlag {
				mux.ServeHTTP(w, r)
			} else {
				http.Error(w, "API not enabled", http.StatusNotFound)
			}
			return
		}

		if *enableDetection && suspiciousRating != nil {
			ip := r.RemoteAddr
			if suspiciousRating.DetectAttack(r) {
				suspiciousRating.UpdateRating(ip, 5)
			}
			rating := suspiciousRating.GetRating(ip)
			if rating > suspiciousRating.maxSuspicion {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			suspiciousRating.UpdateRating(ip, 1)
		}
		logRequest(r)

		proxy := proxyManager.GetProxy()
		if proxy == nil {
			http.Error(w, "No active proxy available", http.StatusServiceUnavailable)
			return
		}

		targetURL := proxy.String() + r.URL.RequestURI()
		http.Redirect(w, r, targetURL, http.StatusTemporaryRedirect)
	})))

	server := &http.Server{
		Addr:         "0.0.0.0:443",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	if *domain != "" {
		logInfo("Starting HTTPS server with custom domain %s", *domain)
		if err := server.ListenAndServeTLS(*certFile, *keyFile); err != nil {
			log.Fatalf("Failed to start HTTPS server: %v", err)
		}
	} else {
		logInfo("Starting HTTP to HTTPS redirect server")
		logInfo("Attempting to start HTTPS server with TLS configuration")
		if err := server.ListenAndServeTLS("server.crt", "server.key"); err != nil {
			log.Fatalf("Failed to start HTTPS server: %v", err)
		}
	}
}

func startConsumers(queue *Queue) {
	for {
		messages, err := queue.ConsumeFromQueue("consumer1", 10, 5*time.Second)
		if err != nil {
			continue
		}
		for _, message := range messages {
			logInfo("Processing message: %v", message.Values)
			err := queue.AckMessage(message.ID)
			if err != nil {
				logError("Failed to acknowledge message: %v", err)
			}
		}
	}
}
