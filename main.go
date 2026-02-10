package main

import (
	"encoding/json"
	"fmt"
	"io"
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
			Timeout: 12 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		userAgents: []string{
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
			"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
			"Mozilla/5.0 (Android 14; Mobile; rv:128.0) Gecko/128.0 Firefox/128.0",
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

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
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
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", referer)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ajax %d for %s", resp.StatusCode, url)
	}

	return io.ReadAll(resp.Body)
}

// ========== SCRAPING FUNCTIONS ==========

func (s *ScraperEngine) GetTrending() ([]Anime, error) {
	doc, err := s.getDocument(BaseURL + "/")
	if err != nil {
		return nil, err
	}

	var items []Anime
	doc.Find("#anime-trending .item, .film_list-wrap .flw-item").Each(func(_ int, sel *goquery.Selection) {
		title := strings.TrimSpace(sel.Find(".film-name, .number .film-title").Text())
		href, _ := sel.Find("a").Attr("href")
		poster, _ := sel.Find("img").Attr("data-src")

		id := ""
		if href != "" {
			parts := strings.Split(strings.Trim(href, "/"), "/")
			if len(parts) > 0 {
				id = parts[len(parts)-1]
			}
		}

		if title != "" && id != "" {
			items = append(items, Anime{Title: title, ID: id, Poster: poster})
		}
	})
	return items, nil
}

func (s *ScraperEngine) GetSidebarList(listType string) ([]Anime, error) {
	doc, err := s.getDocument(BaseURL + "/")
	if err != nil {
		return nil, err
	}

	var items []Anime
	searchTerm := strings.ToLower(strings.ReplaceAll(listType, "-", " "))

	doc.Find(".block_area-realtime").Each(func(_ int, block *goquery.Selection) {
		header := strings.ToLower(strings.TrimSpace(block.Find(".main-heading").Text()))
		if strings.Contains(header, searchTerm) {
			block.Find("ul li .flw-item").Each(func(_ int, li *goquery.Selection) {
				title := strings.TrimSpace(li.Find(".film-name").Text())
				href, _ := li.Find("a").Attr("href")
				poster, _ := li.Find("img").Attr("data-src")

				id := ""
				if href != "" {
					parts := strings.Split(strings.Trim(href, "/"), "/")
					if len(parts) > 0 {
						id = parts[len(parts)-1]
					}
				}

				if title != "" && id != "" {
					items = append(items, Anime{Title: title, ID: id, Poster: poster})
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
		return nil, err
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
	ajaxURL := fmt.Sprintf("%s/ajax/v2/episode/list/%s", BaseURL, animeID)
	referer := fmt.Sprintf("%s/watch/%s", BaseURL, animeID)

	body, err := s.getAjax(ajaxURL, referer)
	if err != nil {
		return nil, err
	}

	var ajaxResp struct {
		Status bool   `json:"status"`
		HTML   string `json:"html"`
	}

	if err := json.Unmarshal(body, &ajaxResp); err != nil {
		log.Printf("Episode AJAX JSON error for %s: %v - raw: %s", animeID, err, string(body[:200]))
		return nil, fmt.Errorf("invalid episode ajax response")
	}

	if !ajaxResp.Status || ajaxResp.HTML == "" {
		return nil, fmt.Errorf("no episode data in ajax response")
	}

	fragment, err := goquery.NewDocumentFromReader(strings.NewReader(ajaxResp.HTML))
	if err != nil {
		return nil, err
	}

	var episodes []Episode
	fragment.Find("a").Each(func(_ int, sel *goquery.Selection) {
		epNum := strings.TrimSpace(sel.Find(".ep-num, .ssli-detail, .number").Text())
		if epNum == "" {
			epNum, _ = sel.Attr("data-number")
			if epNum == "" {
				epNum = strings.TrimSpace(sel.Text())
			}
		}

		title, _ := sel.Attr("title")
		if title == "" {
			title = sel.Find(".name, .ep-title").Text()
		}

		id, _ := sel.Attr("data-id")
		if id == "" {
			href, _ := sel.Attr("href")
			if href != "" {
				parts := strings.Split(strings.Trim(href, "/"), "/")
				if len(parts) > 0 {
					id = parts[len(parts)-1]
				}
			}
		}

		epNum = strings.TrimPrefix(strings.ToUpper(epNum), "EP ")
		epNum = strings.TrimSpace(epNum)

		if id != "" && epNum != "" {
			episodes = append(episodes, Episode{
				ID:     id,
				Number: epNum,
				Title:  title,
			})
		}
	})

	log.Printf("Found %d episodes for anime %s", len(episodes), animeID)
	return episodes, nil
}

func (s *ScraperEngine) GetRecommendations(animeID string, limit int) ([]Anime, error) {
	watchURL := BaseURL + "/watch/" + animeID
	doc, err := s.getDocument(watchURL)
	if err != nil {
		return nil, err
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
		topAiring, _ := s.GetSidebarList("top airing")
		mostPopular, _ := s.GetSidebarList("most popular")
		mostFavorite, _ := s.GetSidebarList("most favorite")

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
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
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

		data, err := s.GetEpisodes(id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "success", "episodes": data})
	}
}

func recommendationsHandler(s *ScraperEngine) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		limit := 12
		if l := c.Query("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
		}

		data, err := s.GetRecommendations(id, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, RecommendationResponse{Status: "success", Data: data})
	}
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "healthy", "version": "1.2.0"})
}

func main() {
	scraper := NewScraperEngine()
	gin.SetMode(gin.ReleaseMode)

	router := gin.Default()
	router.Use(cors.Default())
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
			"endpoints": []string{"/api/discover", "/api/franchise", "/api/episodes/:id", "/api/recommendations/:id"},
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting server on port %s", port)
	if err := http.ListenAndServe(":"+port, router); err != nil {
		log.Fatal(err)
	}
}
