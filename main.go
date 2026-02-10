package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"golang.org/x/time/rate"
)

// ========== CONFIGURATION ==========
const (
	BaseURL = "https://hianime.to"
)

// ========== DATA STRUCTURES ==========
type Anime struct {
	Rank  string `json:"rank"`
	Title string `json:"title"`
	ID    string `json:"id"`
	URL   string `json:"url,omitempty"`
}

type DiscoveryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Trending     []Anime `json:"trending"`
		TopAiring    []Anime `json:"top_airing"`
		MostPopular  []Anime `json:"most_popular"`
		MostFavorite []Anime `json:"most_favorite"`
	} `json:"data"`
}

// ========== CACHE & RATE LIMITING ==========
var (
	// In-memory cache (12 hours for discover endpoint)
	memoryCache = cache.New(12*time.Hour, 24*time.Hour)

	// Rate limiting: 100 requests per minute per IP
	rateLimiter = struct {
		ips map[string]*rate.Limiter
		mu  sync.RWMutex
	}{
		ips: make(map[string]*rate.Limiter),
	}
)

// ========== SCRAPER ENGINE ==========
type ScraperEngine struct {
	client     *http.Client
	userAgents []string
}

func NewScraperEngine() *ScraperEngine {
	return &ScraperEngine{
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		userAgents: []string{
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36",
			"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:109.0) Gecko/20100101 Firefox/119.0",
		},
	}
}

func (s *ScraperEngine) getDocument(url string) (*goquery.Document, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Random user agent
	req.Header.Set("User-Agent", s.userAgents[time.Now().UnixNano()%int64(len(s.userAgents))])
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status code error: %d %s", resp.StatusCode, resp.Status)
	}

	return goquery.NewDocumentFromReader(resp.Body)
}

func (s *ScraperEngine) GetTrending() ([]Anime, error) {
	doc, err := s.getDocument(BaseURL + "/home")
	if err != nil {
		return nil, err
	}

	var trending []Anime

	// Scrape trending anime
	doc.Find("#anime-trending .item").Each(func(i int, sel *goquery.Selection) {
		rankElem := sel.Find(".number span")
		titleElem := sel.Find(".number .film-title")
		linkElem := sel.Find(".number a")

		if titleElem.Length() > 0 {
			rank := "N/A"
			if rankElem.Length() > 0 {
				rank = strings.TrimSpace(rankElem.Text())
			}

			title := strings.TrimSpace(titleElem.Text())
			id := ""

			if linkElem.Length() > 0 {
				href, exists := linkElem.Attr("href")
				if exists {
					parts := strings.Split(href, "/")
					if len(parts) > 0 {
						id = parts[len(parts)-1]
					}
				}
			}

			trending = append(trending, Anime{
				Rank:  rank,
				Title: title,
				ID:    id,
				URL:   BaseURL + "/watch/" + id,
			})
		}
	})

	return trending, nil
}

func (s *ScraperEngine) GetSidebarList(listType string) ([]Anime, error) {
	doc, err := s.getDocument(BaseURL + "/home")
	if err != nil {
		return nil, err
	}

	var results []Anime

	searchTerm := strings.ReplaceAll(listType, "-", " ")

	// Find the correct block
	doc.Find(".block_area-realtime").Each(func(i int, block *goquery.Selection) {
		header := block.Find(".main-heading")
		if header.Length() > 0 {
			headerText := strings.ToLower(header.Text())
			if strings.Contains(headerText, searchTerm) {
				block.Find("ul li").Each(func(j int, item *goquery.Selection) {
					nameElem := item.Find(".film-name a")
					if nameElem.Length() > 0 {
						title := strings.TrimSpace(nameElem.Text())
						id := ""

						href, exists := nameElem.Attr("href")
						if exists {
							parts := strings.Split(href, "/")
							if len(parts) > 0 {
								id = parts[len(parts)-1]
							}
						}

						rank := "N/A"
						rankElem := item.Find(".number span")
						if rankElem.Length() > 0 {
							rank = strings.TrimSpace(rankElem.Text())
						}

						results = append(results, Anime{
							Rank:  rank,
							Title: title,
							ID:    id,
							URL:   BaseURL + "/watch/" + id,
						})
					}
				})
			}
		}
	})

	return results, nil
}

// ========== MIDDLEWARE ==========
func rateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()

		rateLimiter.mu.Lock()
		limiter, exists := rateLimiter.ips[ip]
		if !exists {
			// 100 requests per minute
			limiter = rate.NewLimiter(rate.Every(time.Minute), 100)
			rateLimiter.ips[ip] = limiter
		}
		rateLimiter.mu.Unlock()

		if !limiter.Allow() {
			c.JSON(429, gin.H{
				"status":  "error",
				"message": "Too many requests. Please slow down.",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

func cacheMiddleware(duration time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only cache GET requests
		if c.Request.Method != "GET" {
			c.Next()
			return
		}

		// Create cache key
		cacheKey := c.Request.URL.String()

		// Try to get from cache
		if cached, found := memoryCache.Get(cacheKey); found {
			log.Printf("Cache hit for: %s", cacheKey)
			c.Data(200, "application/json", cached.([]byte))
			c.Abort()
			return
		}

		// Replace writer to capture response
		writer := &responseWriter{
			ResponseWriter: c.Writer,
			body:           []byte{},
		}
		c.Writer = writer

		c.Next()

		// Only cache successful responses
		if c.Writer.Status() == 200 && len(writer.body) > 0 {
			memoryCache.Set(cacheKey, writer.body, duration)
			log.Printf("Cached response for: %s (duration: %v)", cacheKey, duration)
		}
	}
}

// Custom response writer to capture response
type responseWriter struct {
	gin.ResponseWriter
	body []byte
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.body = b
	return w.ResponseWriter.Write(b)
}

// ========== API HANDLERS ==========
func discoverHandler(scraper *ScraperEngine) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Try to get from cache first (handled by middleware)
		// If not cached, scrape fresh data

		trending, err1 := scraper.GetTrending()
		topAiring, err2 := scraper.GetSidebarList("top-airing")
		mostPopular, err3 := scraper.GetSidebarList("most-popular")
		mostFavorite, err4 := scraper.GetSidebarList("most-favorite")

		// Check for errors
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			c.JSON(500, gin.H{
				"status":  "error",
				"message": "Failed to scrape data",
			})
			return
		}

		response := DiscoveryResponse{
			Status: "success",
		}
		response.Data.Trending = trending
		response.Data.TopAiring = topAiring
		response.Data.MostPopular = mostPopular
		response.Data.MostFavorite = mostFavorite

		c.JSON(200, response)
	}
}

func healthHandler(c *gin.Context) {
	c.JSON(200, gin.H{
		"status":  "healthy",
		"service": "Go Anime API",
		"version": "1.0.0",
	})
}

func clearCacheHandler(c *gin.Context) {
	// Optional: Add authentication for this endpoint in production
	memoryCache.Flush()
	c.JSON(200, gin.H{
		"status":  "success",
		"message": "Cache cleared",
	})
}

// ========== MAIN FUNCTION ==========
func main() {
	// Initialize scraper
	scraper := NewScraperEngine()

	// Set Gin mode
	gin.SetMode(gin.ReleaseMode)

	// Create router
	router := gin.Default()

	// CORS configuration (same as Flask-CORS)
	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// Global middleware
	router.Use(
		rateLimitMiddleware(), // Rate limiting
		gin.Logger(),          // Logging
		gin.Recovery(),        // Recovery from panics
	)

	// Routes
	api := router.Group("/api")
	{
		// Discover endpoint with 12-hour cache
		api.GET("/discover", cacheMiddleware(12*time.Hour), discoverHandler(scraper))

		// Health check (no cache)
		api.GET("/health", healthHandler)

		// Admin endpoints (optional)
		admin := api.Group("/admin")
		{
			admin.POST("/cache/clear", clearCacheHandler)
		}
	}

	// Root endpoint
	router.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"service": "Anime Discovery API",
			"version": "1.0.0",
			"endpoints": []string{
				"GET /api/discover - Get trending anime (cached 12h)",
				"GET /api/health - Health check",
				"POST /api/admin/cache/clear - Clear cache (admin)",
			},
			"documentation": "https://github.com/yourusername/anivoid-api2",
		})
	})

	// Get PORT from environment variables (Required for Render)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Default port if not set
	}

	// Print startup message
	fmt.Println("┌─────────────────────────────────────────────────────┐")
	fmt.Println("│               ANIVOID API 2                         │")
	fmt.Println("├─────────────────────────────────────────────────────┤")
	fmt.Printf("│  Server:  http://0.0.0.0:%s                       │\n", port)
	fmt.Println("│  Status:  ✅ Running                               │")
	fmt.Println("│  Cache:   ✅ 12-hour in-memory cache               │")
	fmt.Println("│  Rate Limit: 100 requests/minute per IP             │")
	fmt.Println("│  Source: https://hianime.to                         │")
	fmt.Println("├─────────────────────────────────────────────────────┤")
	fmt.Println("│  Endpoints:                                         │")
	fmt.Println("│  • GET /api/discover - Trending anime               │")
	fmt.Println("│  • GET /api/health - Health check                   │")
	fmt.Println("│  • GET / - API documentation                        │")
	fmt.Println("└─────────────────────────────────────────────────────┘")

	// Start server
	log.Printf("Starting server on port %s", port)

	// Production-ready HTTP server configuration
	server := &http.Server{
		Addr:         ":" + port, // Listen on all interfaces
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil {
		log.Fatal("Server failed to start: ", err)
	}

}
