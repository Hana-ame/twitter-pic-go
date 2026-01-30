package twitter

import (
	"bufio"
	"log"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/phemmer/go-iptrie" // Optimized Radix Trie for IPs
)

type BanManager struct {
	// atomic.Pointer ensures thread-safe "hot-swapping" of the ban list
	trie atomic.Pointer[iptrie.Trie]
}

func NewBanManager() *BanManager {
	bm := &BanManager{}
	bm.Reload([]string{}) // Initialize empty
	return bm
}

// Reload "compiles" the string list into an optimized Radix Tree
func (bm *BanManager) Reload(networks []string) error {
	newTrie := iptrie.NewTrie()
	for _, s := range networks {
		prefix, err := parseCIDR(s)
		if err != nil {
			return err
		}
		// We store an empty struct{} as the value to save memory
		newTrie.Insert(prefix, struct{}{})
	}
	bm.trie.Store(newTrie)
	return nil
}

func (bm *BanManager) IsBanned(ipStr string) bool {
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false
	}
	t := bm.trie.Load()
	if t == nil {
		return false
	}
	return t.Contains(ip)
}

// Helper to handle both "1.1.1.1" and "1.1.0.0/24"
func parseCIDR(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// gin

// func IPBanMiddleware(bm *BanManager) gin.HandlerFunc {
// 	return func(c *gin.Context) {
// 		// c.ClientIP() parses X-Forwarded-For based on your TrustedProxies config
// 		clientIP := c.ClientIP()

// 		if bm.IsBanned(clientIP) {
// 			c.AbortWithStatus(http.StatusForbidden)
// 			return
// 		}

// 		c.Next()
// 	}
// }

// from file

func NewBanManagerFromFile(filePath string) *BanManager {
	bm := &BanManager{}
	if err := bm.ReloadFromFile(filePath); err != nil {
		log.Printf("Initial ban list load failed (starting empty): %v", err)
		bm.trie.Store(iptrie.NewTrie())
	}
	return bm
}

// ReloadFromFile reads the file, "compiles" a new Trie, and swaps it atomically.
func (bm *BanManager) ReloadFromFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	newTrie := iptrie.NewTrie()
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines or comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		prefix, err := parseToPrefix(line)
		if err != nil {
			log.Printf("Warning: skipping invalid IP/CIDR in ban file: %s", line)
			continue
		}
		newTrie.Insert(prefix, struct{}{})
	}

	// Swap the pointer: Lookups are never blocked
	bm.trie.Store(newTrie)
	return nil
}

func (bm *BanManager) IsBannedAddr(ip netip.Addr) bool {
	t := bm.trie.Load()
	if t == nil {
		return false
	}
	return t.Contains(ip)
}

func parseToPrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// func IPBanMiddleware(bm *BanManager) gin.HandlerFunc {
// 	return func(c *gin.Context) {
// 		// 1. Get X-Forwarded-For: "RealIP, CloudflareIP"
// 		xff := c.GetHeader("X-Forwarded-For")
// 		var clientIPStr string

// 		if xff != "" {
// 			// Take the first one (leftmost)
// 			i := strings.Index(xff, ",")
// 			if i != -1 {
// 				clientIPStr = strings.TrimSpace(xff[:i])
// 			} else {
// 				clientIPStr = strings.TrimSpace(xff)
// 			}
// 		} else {
// 			clientIPStr = c.RemoteIP()
// 		}

// 		// 2. High-speed Radix Tree Lookup
// 		ip, err := netip.ParseAddr(clientIPStr)
// 		if err == nil && bm.IsBanned(ip) {
// 			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
// 				"error": "Forbidden",
// 				"ip":    clientIPStr,
// 			})
// 			return
// 		}

// 		c.Next()
// 	}
// }

func StrictIPBanMiddleware(bm *BanManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. Check physical connection IP (RemoteAddr)
		// RemoteIP() returns the physical IP if no trusted proxies are set
		remoteAddr, _ := netip.ParseAddr(c.RemoteIP())
		if remoteAddr.IsValid() && bm.IsBannedAddr(remoteAddr) {
			abortBanned(c, remoteAddr.String())
			return
		}

		// 2. Check every IP in X-Forwarded-For (XFF)
		// Format: "UserFakeIP, RealIP, CloudflareIP"
		xff := c.GetHeader("X-Forwarded-For")
		if xff != "" {
			ips := strings.Split(xff, ",")
			for _, ipStr := range ips {
				trimmedIP := strings.TrimSpace(ipStr)
				if trimmedIP == "" {
					continue
				}

				addr, err := netip.ParseAddr(trimmedIP)
				if err != nil {
					continue // Ignore malformed IPs in the header
				}

				if bm.IsBannedAddr(addr) {
					// Short-circuit: stop on first banned IP found
					abortBanned(c, trimmedIP)
					return
				}
			}
		}

		c.Next()
	}
}

func abortBanned(c *gin.Context, bannedIP string) {
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error":  "Access Denied",
		"reason": "Banned IP detected in chain",
		"ip":     bannedIP,
	})
}
