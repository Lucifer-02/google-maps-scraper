package gmaps

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/google/uuid"
	"github.com/gosom/google-maps-scraper/deduper"
	"github.com/gosom/google-maps-scraper/exiter"
	"github.com/gosom/scrapemate"
	"github.com/playwright-community/playwright-go"
)

type GmapJobOptions func(*GmapJob)

type GmapJob struct {
	scrapemate.Job

	MaxDepth     int
	LangCode     string
	ExtractEmail bool

	Deduper             deduper.Deduper
	ExitMonitor         exiter.Exiter
	ExtractExtraReviews bool
}

func NewGmapJob(
	id, langCode, query string,
	maxDepth int,
	extractEmail bool,
	geoCoordinates string,
	zoom int,
	opts ...GmapJobOptions,
) *GmapJob {
	query = url.QueryEscape(query)

	const (
		maxRetries = 3
		prio       = scrapemate.PriorityLow
	)

	if id == "" {
		id = uuid.New().String()
	}

	mapURL := ""
	if geoCoordinates != "" && zoom > 0 {
		mapURL = fmt.Sprintf("https://www.google.com/maps/search/%s/@%s,%dz", query, strings.ReplaceAll(geoCoordinates, " ", ""), zoom)
	} else {
		//Warning: geo and zoom MUST be both set or not
		mapURL = fmt.Sprintf("https://www.google.com/maps/search/%s", query)
	}

	job := GmapJob{
		Job: scrapemate.Job{
			ID:         id,
			Method:     http.MethodGet,
			URL:        mapURL,
			URLParams:  map[string]string{"hl": langCode},
			MaxRetries: maxRetries,
			Priority:   prio,
		},
		MaxDepth:     maxDepth,
		LangCode:     langCode,
		ExtractEmail: extractEmail,
	}

	for _, opt := range opts {
		opt(&job)
	}

	return &job
}

func WithDeduper(d deduper.Deduper) GmapJobOptions {
	return func(j *GmapJob) {
		j.Deduper = d
	}
}

func WithExitMonitor(e exiter.Exiter) GmapJobOptions {
	return func(j *GmapJob) {
		j.ExitMonitor = e
	}
}

func WithExtraReviews() GmapJobOptions {
	return func(j *GmapJob) {
		j.ExtractExtraReviews = true
	}
}

func (j *GmapJob) UseInResults() bool {
	return false
}

func (j *GmapJob) Process(ctx context.Context, resp *scrapemate.Response) (any, []scrapemate.IJob, error) {
	defer func() {
		// Help the GC by releasing references to large objects.
		resp.Document = nil
		resp.Body = nil
	}()

	log := scrapemate.GetLoggerFromContext(ctx)

	doc, ok := resp.Document.(*goquery.Document)
	if !ok {
		return nil, nil, fmt.Errorf("could not convert to goquery document")
	}

	var next []scrapemate.IJob

	if strings.Contains(resp.URL, "/maps/place/") {
		jopts := []PlaceJobOptions{}
		if j.ExitMonitor != nil {
			jopts = append(jopts, WithPlaceJobExitMonitor(j.ExitMonitor))
		}
		placeJob := NewPlaceJob(j.ID, j.LangCode, resp.URL, j.ExtractEmail, j.ExtractExtraReviews, jopts...)
		next = append(next, placeJob)
	} else {
		doc.Find(`div[role=feed] div[jsaction]>a`).Each(func(_ int, s *goquery.Selection) {
			if href := s.AttrOr("href", ""); href != "" {
				jopts := []PlaceJobOptions{}
				if j.ExitMonitor != nil {
					jopts = append(jopts, WithPlaceJobExitMonitor(j.ExitMonitor))
				}
				nextJob := NewPlaceJob(j.ID, j.LangCode, href, j.ExtractEmail, j.ExtractExtraReviews, jopts...)
				if j.Deduper == nil || j.Deduper.AddIfNotExists(ctx, href) {
					next = append(next, nextJob)
				}
			}
		})
	}

	if j.ExitMonitor != nil {
		j.ExitMonitor.IncrPlacesFound(len(next))
		j.ExitMonitor.IncrSeedCompleted(1)
	}

	log.Info(fmt.Sprintf("%d places found", len(next)))

	return nil, next, nil
}

func (j *GmapJob) BrowserActions(ctx context.Context, page playwright.Page) scrapemate.Response {
	// IMPROVEMENT: Ensure the page is closed to prevent resource leaks.
	// While the framework calling this function should handle page closing,
	// adding a defer here acts as a critical safeguard against leaks if
	// an error or panic occurs within this function's scope.
	defer page.Close()

	var resp scrapemate.Response

	// IMPROVEMENT: Derive timeout from the context for all browser operations.
	// This makes the job respect the overall deadline.
	pageResponse, err := page.Goto(j.GetFullURL(), playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(timeoutFromContext(ctx, 30000)), // Default 30s
	})
	if err != nil {
		resp.Error = err
		return resp
	}

	if err = clickRejectCookiesIfRequired(ctx, page); err != nil {
		resp.Error = err
		return resp
	}

	// Wait for any potential redirects after cookie handling to settle.
	err = page.WaitForURL(page.URL(), playwright.PageWaitForURLOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(timeoutFromContext(ctx, 5000)), // Default 5s
	})
	if err != nil {
		resp.Error = err
		return resp
	}

	resp.URL = pageResponse.URL()
	resp.StatusCode = pageResponse.Status()
	resp.Headers = make(http.Header, len(pageResponse.Headers()))
	for k, v := range pageResponse.Headers() {
		resp.Headers.Add(k, v)
	}

	// When Google Maps finds only 1 place, it slowly redirects to that place's URL.
	// We first attempt a quick check for the main results feed.
	sel := `div[role='feed']`
	_, err = page.WaitForSelector(sel, playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(700), // Keep this short; a failure is expected for single results.
	})

	var singlePlace bool
	if err != nil {
		// If the feed isn't found quickly, it might be a redirect to a single place page.
		// IMPROVEMENT: Use a context-aware wait to prevent goroutine leaks.
		singlePlace = waitUntilURLContains(ctx, page, "/maps/place/")
	}

	if singlePlace {
		resp.URL = page.URL()
		body, err := page.Content()
		if err != nil {
			resp.Error = err
			return resp
		}
		resp.Body = []byte(body)
		return resp
	}

	// If it's a list of results, scroll to load all of them.
	scrollSelector := `div[role='feed']`
	if _, err = scroll(ctx, page, j.MaxDepth, scrollSelector); err != nil {
		resp.Error = err
		return resp
	}

	body, err := page.Content()
	if err != nil {
		resp.Error = err
		return resp
	}
	resp.Body = []byte(body)

	return resp
}

// IMPROVEMENT: This function is now fully context-aware to prevent goroutine leaks.
// It stops immediately if the context is canceled.
func waitUntilURLContains(ctx context.Context, page playwright.Page, s string) bool {
	ticker := time.NewTicker(time.Millisecond * 150)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done(): // Immediately exit if the context is canceled.
			return false
		case <-ticker.C:
			if strings.Contains(page.URL(), s) {
				return true
			}
		}
	}
}

// IMPROVEMENT: The function now accepts a context to derive its timeout,
// making it more robust and preventing it from blocking indefinitely.
func clickRejectCookiesIfRequired(ctx context.Context, page playwright.Page) error {
	sel := `form[action="https://consent.google.com/save"]:first-of-type button:first-of-type`

	el, err := page.WaitForSelector(sel, playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(timeoutFromContext(ctx, 3000)), // Default 3s
	})

	// If there's an error (like a timeout), it means the element was not found, which is not a failure.
	if err != nil {
		return nil
	}
	if el == nil {
		return nil
	}

	return el.Click()
}

// IMPROVEMENT: Replaced blocking `page.WaitForTimeout` with a context-aware wait.
// This ensures the scroll loop can be interrupted if the job is canceled.
func scroll(ctx context.Context, page playwright.Page, maxDepth int, scrollSelector string) (int, error) {
	expr := `async () => {
		const el = document.querySelector("` + scrollSelector + `");
		if (!el) return 0;
		el.scrollTop = el.scrollHeight;
		return new Promise(resolve => setTimeout(() => resolve(el.scrollHeight), 200));
	}`

	var currentScrollHeight int
	const maxWait = 2000 * time.Millisecond
	cnt := 0

	for i := 0; i < maxDepth; i++ {
		select {
		case <-ctx.Done():
			return cnt, ctx.Err()
		default:
		}

		cnt++

		scrollHeight, err := page.Evaluate(expr)
		if err != nil {
			return cnt, err
		}

		height, ok := scrollHeight.(int)
		if !ok {
			return cnt, fmt.Errorf("scrollHeight is not an int")
		}

		if height == currentScrollHeight && height > 0 {
			// If scroll height hasn't changed, we've reached the end.
			break
		}
		currentScrollHeight = height

		// Create a context-aware delay instead of a blocking sleep.
		waitTime := time.Duration(150*cnt) * time.Millisecond
		if waitTime > maxWait {
			waitTime = maxWait
		}

		select {
		case <-time.After(waitTime):
			// Continue loop
		case <-ctx.Done():
			return cnt, ctx.Err()
		}
	}

	return cnt, nil
}

// timeoutFromContext is a helper to calculate the timeout in milliseconds
// based on the context's deadline. It returns a default if no deadline is set.
func timeoutFromContext(ctx context.Context, defaultTimeoutMs float64) float64 {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			return float64(remaining.Milliseconds())
		}
		// If deadline has passed, return a very small timeout to fail fast.
		return 1
	}
	return defaultTimeoutMs
}

// Dummy PlaceJob for compilability
// type PlaceJobOptions func(*PlaceJob)
// type PlaceJob struct{ scrapemate.Job }
//
// func NewPlaceJob(id, lang, url string, extractEmail, extraReviews bool, opts ...PlaceJobOptions) *PlaceJob {
// 	return &PlaceJob{}
// }
// func WithPlaceJobExitMonitor(e exiter.Exiter) PlaceJobOptions {
// 	return func(j *PlaceJob) {}
// }
