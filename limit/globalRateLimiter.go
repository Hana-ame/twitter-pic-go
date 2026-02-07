import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// GlobalLimiter: 80 requests per hour, with a burst of 80
// (Burst of 80 allows the "bucket" to fill up to 80 if unused)
var globalLimiter = rate.NewLimiter(rate.Every(time.Hour/80), 80)

func GlobalRateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. Check if the skip parameter is present
		if c.Query("do_not_renew") == "true" {
			c.Next()
			return
		}

		// 2. Apply the global limit
		if !globalLimiter.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "Global rate limit exceeded (80 req/hour).",
			})
			return
		}

		c.Next()
	}
}
