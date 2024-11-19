package main

import (
	"crypto/tls"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/go-redis/redis/v8"
	"golang.org/x/exp/rand"
)

// startAutoSwitch switches proxies automatically every 10 seconds
func (pm *ProxyManager) startAutoSwitch() {
	for range pm.ticker.C {
		pm.switchProxy()
	}
}

// switchProxy switches to the next proxy in the list
func (pm *ProxyManager) switchProxy() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	rand.Shuffle(len(pm.proxies), func(i, j int) {
		pm.proxies[i], pm.proxies[j] = pm.proxies[j], pm.proxies[i]
	})

	pm.currentProxy = pm.proxies[0]
	log.Printf("Switched to new proxy: %s", pm.currentProxy)

	// Incrémentez la métrique pour le proxy actif
	proxySwitchesTotal.WithLabelValues(pm.currentProxy.String()).Inc()

	pm.UpdateActiveProxy(pm.currentProxy)
}

// GetProxy returns the current proxy
func (pm *ProxyManager) GetProxy() *url.URL {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.currentProxy
}

// UpdateActiveProxy updates the active proxy in Redis
func (pm *ProxyManager) UpdateActiveProxy(currentProxy *url.URL) {
	redisClient := redis.NewClient(&redis.Options{
		Addr: "localhost:6379", // Adresse de Redis
	})
	defer redisClient.Close()

	// Met à jour l'URL du proxy actif dans Redis
	err := redisClient.Set(ctx, "active_proxy", currentProxy.String(), 0).Err()
	if err != nil {
		log.Printf("Failed to update active proxy in Redis: %v", err)
	} else {
		log.Printf("Successfully updated active proxy to %s in Redis", currentProxy.String())
	}

	// Publie une mise à jour sur le canal "proxy_updates"
	err = redisClient.Publish(ctx, "proxy_updates", currentProxy.String()).Err()
	if err != nil {
		log.Printf("Failed to publish proxy update: %v", err)
	}
}

// GetActiveProxy retrieves the currently active proxy from Redis
func (pm *ProxyManager) GetActiveProxy() (*url.URL, error) {
	redisClient := redis.NewClient(&redis.Options{
		Addr: "localhost:6379", // Adresse de Redis
	})
	defer redisClient.Close()

	activeProxyStr, err := redisClient.Get(ctx, "active_proxy").Result()
	if err != nil {
		return nil, err
	}

	activeProxy, err := url.Parse(activeProxyStr)
	if err != nil {
		return nil, err
	}

	return activeProxy, nil
}

func isProxyHealthy(proxyURL string) bool {
	client := http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Ignore les erreurs TLS
		},
	}
	resp, err := client.Get(proxyURL + "/health")
	if err != nil {
		log.Printf("Health check failed for proxy %s: %v", proxyURL, err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Health check failed for proxy %s: status %d", proxyURL, resp.StatusCode)
		return false
	}

	log.Printf("Proxy %s is healthy", proxyURL)
	return true
}

func getNewProxyURL() string {
	// Retourner le proxy actif actuel
	return currentProxyURL
}
