package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
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
	Rank   string `json:"rank"`
	Title  string `json:"title"`
	ID     string `json:"id"`
	Poster string `json:"poster,omitempty"`
	URL    string `json:"url,omitempty"`
}

type FranchiseItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Poster string `json:"poster,omitempty"`
	AlID   string `json:"al_id,omitempty"`
}

type Episode struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	Title  string `json:"title"`
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

type RecommendationResponse struct {
	Status string  `json:"status"`
	Data   []Anime `json:"data"`
}

// ========== CACHE & RATE LIMITING ==========
var (
	memoryCache = cache.New(12*time.Hour, 24*time.Hour)

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
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     60 * time.Second,
			},
		},
		userAgents: []string{
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		},
	}
}

func (s *ScraperEngine) getDocument(url string) (*goquery.Document, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	ua := s.userAgents[time.Now().UnixNano()%int64(len(s.userAgents))]
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", BaseURL)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Failed to fetch page %s: status %d", url, resp.StatusCode)
		return nil, fmt.Errorf("http %d for %s", resp.StatusCode, url)
	}

	return goquery.NewDocumentFromReader(resp.Body)
}

func (s *ScraperEngine) getAjax(url string, referer string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	ua := s.userAgents[time.Now().UnixNano()%int64(len(s.userAgents))]
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", referer)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("AJAX request failed %s: status %d", url, resp.StatusCode)
		return nil, fmt.Errorf("ajax %d for %s", resp.StatusCode, url)
	}

	return io.ReadAll(resp.Body)
}

// ========== SCRAPING FUNCTIONS ==========

func (s *ScraperEngine) GetTrending() ([]Anime, error) {
	doc, err := s.getDocument(BaseURL + "/home")
	if err != nil {
		return nil, err
	}
	var items []Anime
	doc.Find("#anime-trending .item").Each(func(_ int, sel *goquery.Selection) {
		rank := strings.TrimSpace(sel.Find(".number span").Text())
		title := strings.TrimSpace(sel.Find(".number .film-title").Text())
		link := sel.Find(".number a")
		href, exists := link.Attr("href")
		
		if title != "" && exists {
			id := ""
			if href != "" {
				parts := strings.Split(strings.Trim(href, "/"), "/")
				if len(parts) > 0 {
					id = parts[len(parts)-1]
				}
			}
			items = append(items, Anime{
				Rank:   rank,
				Title:  title,
				ID:     id,
			})
		}
	})
	return items, nil
}

func (s *ScraperEngine) GetSidebarList(listType string) ([]Anime, error) {
	doc, err := s.getDocument(BaseURL + "/home")
	if err != nil {
		return nil, err
	}
	var items []Anime
	
	// Convert listType to search term (e.g., "top-airing" -> "top airing")
	searchTerm := strings.ReplaceAll(listType, "-", " ")
	
	doc.Find(".block_area-realtime").Each(func(_ int, block *goquery.Selection) {
		header := strings.TrimSpace(block.Find(".main-heading").Text())
		if strings.Contains(strings.ToLower(header), strings.ToLower(searchTerm)) {
			block.Find("ul li").Each(func(_ int, li *goquery.Selection) {
				nameElem := li.Find(".film-name a")
				if nameElem.Length() > 0 {
					title := strings.TrimSpace(nameElem.Text())
					href, exists := nameElem.Attr("href")
					rank := strings.TrimSpace(li.Find(".number span").Text())
					
					if title != "" && exists {
						id := ""
						if href != "" {
							parts := strings.Split(strings.Trim(href, "/"), "/")
							if len(parts) > 0 {
								id = parts[len(parts)-1]
							}
						}
						items = append(items, Anime{
							Title: title,
							ID:    id,
							Rank:  rank,
						})
					}
				}
			})
		}
	})
	return items, nil
}

func (s *ScraperEngine) SearchAnime(title string) ([]Anime, error) {
	searchURL := fmt.Sprintf("%s/search?keyword=%s", BaseURL, strings.ReplaceAll(title, " ", "+"))
	doc, err := s.getDocument(searchURL)
	if err != nil {
		return nil, err
	}
	var results []Anime
	doc.Find(".film_list-wrap .flw-item").Each(func(_ int, item *goquery.Selection) {
		title := strings.TrimSpace(item.Find(".film-name").Text())
		href, _ := item.Find(".film-poster a").Attr("href")
		poster, _ := item.Find("img").Attr("data-src")
		id := ""
		if href != "" {
			parts := strings.Split(strings.Trim(href, "/"), "/")
			if len(parts) > 0 {
				id = parts[len(parts)-1]
			}
		}
		if title != "" && id != "" {
			results = append(results, Anime{Title: title, ID: id, Poster: poster})
		}
	})
	return results, nil
}

func (s *ScraperEngine) GetFranchise(title string) ([]FranchiseItem, error) {
	results, err := s.SearchAnime(title)
	if err != nil || len(results) == 0 {
		return nil, fmt.Errorf("no search results for %q", title)
	}
	main := results[0]
	watchURL := BaseURL + "/watch/" + main.ID
	doc, err := s.getDocument(watchURL)
	if err != nil {
		return nil, err
	}
	var franchise []FranchiseItem
	franchise = append(franchise, FranchiseItem{ID: main.ID, Title: main.Title, Poster: main.Poster})
	doc.Find(".os-list a, .related .flw-item a").Each(func(_ int, sel *goquery.Selection) {
		href, _ := sel.Attr("href")
		t := strings.TrimSpace(sel.Text())
		if t == "" {
			t = sel.Find(".film-name").Text()
		}
		if href != "" && t != "" {
			parts := strings.Split(strings.Trim(href, "/"), "/")
			id := parts[len(parts)-1]
			if id != main.ID {
				franchise = append(franchise, FranchiseItem{ID: id, Title: t})
			}
		}
	})
	return franchise, nil
}

func (s *ScraperEngine) GetEpisodes(animeID string) ([]Episode, error) {
	log.Printf("Fetching episodes for: %s", animeID)
	
	// Try the AJAX endpoint first
	ajaxURL := fmt.Sprintf("%s/ajax/v2/episode/list/%s", BaseURL, animeID)
	referer := fmt.Sprintf("%s/watch/%s", BaseURL, animeID)
	
	body, ajaxErr := s.getAjax(ajaxURL, referer)
	if ajaxErr != nil {
		log.Printf("AJAX failed for %s: %v", animeID, ajaxErr)
		// Fallback to regular page
		return s.getEpisodesFromPage(animeID)
	}
	
	var ajaxResp struct {
		Status bool   `json:"status"`
		HTML   string `json:"html"`
	}
	
	if err := json.Unmarshal(body, &ajaxResp); err != nil {
		log.Printf("Failed to parse AJAX response for %s: %v", animeID, err)
		return s.getEpisodesFromPage(animeID)
	}
	
	if !ajaxResp.Status || ajaxResp.HTML == "" {
		log.Printf("AJAX response invalid for %s", animeID)
		return s.getEpisodesFromPage(animeID)
	}
	
	// Parse the HTML from AJAX response
	fragment, err := goquery.NewDocumentFromReader(strings.NewReader(ajaxResp.HTML))
	if err != nil {
		log.Printf("Failed to parse AJAX HTML for %s: %v", animeID, err)
		return s.getEpisodesFromPage(animeID)
	}
	
	var episodes []Episode
	episodeNum := 1
	
	fragment.Find("a").Each(func(_ int, sel *goquery.Selection) {
		// Try to get episode number from data-number attribute
		epNum, exists := sel.Attr("data-number")
		if !exists {
			// Try to extract from text
			epNumText := strings.TrimSpace(sel.Text())
			// Extract numeric part
			for _, r := range epNumText {
				if r >= '0' && r <= '9' {
					epNum = string(r)
					// Try to get multi-digit numbers
					nums := ""
					for _, r2 := range epNumText {
						if r2 >= '0' && r2 <= '9' {
							nums += string(r2)
						} else if len(nums) > 0 {
							break
						}
					}
					if len(nums) > 0 {
						epNum = nums
					}
					break
				}
			}
		}
		
		// If still no number, use incremental counter
		if epNum == "" {
			epNum = strconv.Itoa(episodeNum)
		}
		
		// Get episode ID
		epID, _ := sel.Attr("data-id")
		if epID == "" {
			// Try to extract from href
			href, _ := sel.Attr("href")
			if href != "" {
				parts := strings.Split(strings.Trim(href, "/"), "/")
				if len(parts) > 0 {
					epID = parts[len(parts)-1]
				}
			}
		}
		
		// Get episode title
		title, _ := sel.Attr("title")
		if title == "" {
			title = strings.TrimSpace(sel.Text())
		}
		
		if epID != "" && epNum != "" {
			episodes = append(episodes, Episode{
				ID:     epID,
				Number: epNum,
				Title:  title,
			})
			episodeNum++
		}
	})
	
	log.Printf("Found %d episodes via AJAX for %s", len(episodes), animeID)
	
	if len(episodes) == 0 {
		return s.getEpisodesFromPage(animeID)
	}
	
	return episodes, nil
}

func (s *ScraperEngine) getEpisodesFromPage(animeID string) ([]Episode, error) {
	log.Printf("Falling back to page scrape for: %s", animeID)
	
	watchURL := fmt.Sprintf("%s/watch/%s", BaseURL, animeID)
	doc, err := s.getDocument(watchURL)
	if err != nil {
		return nil, err
	}
	
	var episodes []Episode
	
	// Try multiple selectors
	selectors := []string{
		"#detail-ss-list a",
		".ss-list a",
		".episode-list a",
		".flw-item a",
		".episodios a",
		"[data-id]",
		"[href*='/watch/']",
	}
	
	for _, selector := range selectors {
		doc.Find(selector).Each(func(_ int, sel *goquery.Selection) {
			// Skip if this is not an episode link
			href, _ := sel.Attr("href")
			if href != "" && !strings.Contains(href, "/watch/") && !strings.Contains(href, "ep-") {
				return
			}
			
			// Get episode ID
			epID, _ := sel.Attr("data-id")
			if epID == "" {
				if href != "" {
					parts := strings.Split(strings.Trim(href, "/"), "/")
					if len(parts) > 0 {
						epID = parts[len(parts)-1]
					}
				}
			}
			
			// Get episode number
			epNum := ""
			epNum, exists := sel.Attr("data-number")
			if !exists {
				// Try to extract from text
				text := strings.TrimSpace(sel.Text())
				// Look for numbers in text
				for i, r := range text {
					if r >= '0' && r <= '9' {
						// Extract the number
						numStr := ""
						for j := i; j < len(text); j++ {
							if text[j] >= '0' && text[j] <= '9' {
								numStr += string(text[j])
							} else if len(numStr) > 0 {
								break
							}
						}
						if numStr != "" {
							epNum = numStr
							break
						}
					}
				}
			}
			
			// Get episode title
			title, _ := sel.Attr("title")
			if title == "" {
				title = strings.TrimSpace(sel.Text())
			}
			
			if epID != "" && epNum != "" {
				episodes = append(episodes, Episode{
					ID:     epID,
					Number: epNum,
					Title:  title,
				})
			}
		})
		
		if len(episodes) > 0 {
			break
		}
	}
	
	log.Printf("Found %d episodes via page scrape for %s", len(episodes), animeID)
	return episodes, nil
}

func (s *ScraperEngine) GetRecommendations(animeID string, limit int) ([]Anime, error) {
	watchURL := BaseURL + "/watch/" + animeID
	doc, err := s.getDocument(watchURL)
	if err != nil {
		log.Printf("Recommendations fetch failed for %s: %v", animeID, err)
		return []Anime{}, nil
	}
	var recs []Anime
	doc.Find(".block_area-related .flw-item").EachWithBreak(func(_ int, item *goquery.Selection) bool {
		if len(recs) >= limit {
			return false
		}
		title := strings.TrimSpace(item.Find(".film-name").Text())
		poster, _ := item.Find("img").Attr("data-src")
		href, _ := item.Find("a").Attr("href")
		id := ""
		if href != "" {
			parts := strings.Split(strings.Trim(href, "/"), "/")
			if len(parts) > 0 {
				id = parts[len(parts)-1]
			}
		}
		if title != "" && id != "" {
			recs = append(recs, Anime{Title: title, ID: id, Poster: poster})
		}
		return true
	})
	return recs, nil
}

// ========== MIDDLEWARE ==========
func rateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		rateLimiter.mu.Lock()
		limiter, exists := rateLimiter.ips[ip]
		if !exists {
			limiter = rate.NewLimiter(rate.Every(time.Minute), 100)
			rateLimiter.ips[ip] = limiter
		}
		rateLimiter.mu.Unlock()
		if !limiter.Allow() {
			c.JSON(http.StatusTooManyRequests, gin.H{"status": "error", "message": "Too many requests"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func cacheMiddleware(duration time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != "GET" {
			c.Next()
			return
		}
		key := c.Request.URL.String()
		if cached, found := memoryCache.Get(key); found {
			c.Data(http.StatusOK, "application/json", cached.([]byte))
			c.Abort()
			return
		}
		w := &responseWriter{ResponseWriter: c.Writer, body: []byte{}}
		c.Writer = w
		c.Next()
		if c.Writer.Status() == http.StatusOK && len(w.body) > 0 {
			memoryCache.Set(key, w.body, duration)
		}
	}
}

type responseWriter struct {
	gin.ResponseWriter
	body []byte
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return w.ResponseWriter.Write(b)
}

// ========== HANDLERS ==========
func discoverHandler(s *ScraperEngine) gin.HandlerFunc {
	return func(c *gin.Context) {
		trending, _ := s.GetTrending()
		topAiring, _ := s.GetSidebarList("top-airing")
		mostPopular, _ := s.GetSidebarList("most-popular")
		mostFavorite, _ := s.GetSidebarList("most-favorite")
		resp := DiscoveryResponse{Status: "success"}
		resp.Data.Trending = trending
		resp.Data.TopAiring = topAiring
		resp.Data.MostPopular = mostPopular
		resp.Data.MostFavorite = mostFavorite
		c.JSON(http.StatusOK, resp)
	}
}

func franchiseHandler(s *ScraperEngine) gin.HandlerFunc {
	return func(c *gin.Context) {
		title := c.Query("title")
		if title == "" {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "title required"})
			return
		}
		data, err := s.GetFranchise(title)
		if err != nil {
			log.Printf("Franchise error for %q: %v", title, err)
			c.JSON(http.StatusOK, gin.H{"status": "success", "data": []FranchiseItem{}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "success", "data": data})
	}
}

func episodesHandler(s *ScraperEngine) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "id required"})
			return
		}
		log.Printf("Fetching episodes for ID: %s", id)
		eps, err := s.GetEpisodes(id)
		if err != nil {
			log.Printf("Episodes error for %s: %v", id, err)
			c.JSON(http.StatusOK, gin.H{"status": "success", "episodes": []Episode{}})
			return
		}
		log.Printf("Returning %d episodes for %s", len(eps), id)
		c.JSON(http.StatusOK, gin.H{"status": "success", "episodes": eps})
	}
}

func recommendationsHandler(s *ScraperEngine) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		limit := 12
		if l := c.Query("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
		}
		recs, err := s.GetRecommendations(id, limit)
		if err != nil {
			log.Printf("Recommendations error for %s: %v", id, err)
			c.JSON(http.StatusOK, RecommendationResponse{Status: "success", Data: []Anime{}})
			return
		}
		c.JSON(http.StatusOK, RecommendationResponse{Status: "success", Data: recs})
	}
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "healthy", "version": "1.3.0"})
}

func main() {
	scraper := NewScraperEngine()
	gin.SetMode(gin.ReleaseMode)

	router := gin.Default()
	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))
	router.Use(rateLimitMiddleware())
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	api := router.Group("/api")
	{
		api.GET("/discover", cacheMiddleware(12*time.Hour), discoverHandler(scraper))
		api.GET("/franchise", cacheMiddleware(30*time.Minute), franchiseHandler(scraper))
		api.GET("/episodes/:id", cacheMiddleware(15*time.Minute), episodesHandler(scraper))
		api.GET("/recommendations/:id", cacheMiddleware(30*time.Minute), recommendationsHandler(scraper))
		api.GET("/health", healthHandler)
	}

	router.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service":   "AniVoid API",
			"version":   "1.3.0",
			"endpoints": []string{"/api/discover", "/api/franchise", "/api/episodes/:id", "/api/recommendations/:id"},
		})
	})

	// Test endpoint for debugging
	router.GET("/test/episodes/:id", func(c *gin.Context) {
		id := c.Param("id")
		log.Printf("Test endpoint called for ID: %s", id)
		eps, err := scraper.GetEpisodes(id)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"status": "error", "message": err.Error(), "episodes": []Episode{}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "success", "count": len(eps), "episodes": eps})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting AniVoid API on :%s", port)
	log.Printf("Base URL: %s", BaseURL)
	if err := http.ListenAndServe(":"+port, router); err != nil {
		log.Fatal("Server failed:", err)
	}
}
