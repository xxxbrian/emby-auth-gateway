package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultIncludeItemTypes = "Movie,Series,Season,Episode,BoxSet,Video"
	imageValidationTailSize = 12
)

type config struct {
	GatewayURL       string
	CDNURL           string
	Username         string
	Password         string
	Widths           []int
	Quality          int
	Concurrency      int
	PageSize         int
	MinPageSize      int
	LimitItems       int
	LimitNames       int
	LimitURLs        int
	DBPath           string
	ReportPath       string
	DryRun           bool
	IncludeItemTypes string
	IncludeNames     bool
	RefreshDone      bool
	Timeout          time.Duration
	MetadataRetries  int
	RetryDelay       time.Duration
	ProgressInterval time.Duration
}

type metadataStatusError struct {
	StatusCode int
	URL        string
	Body       string
	RetryAfter time.Duration
}

func (e metadataStatusError) Error() string {
	return fmt.Sprintf("metadata status %d for %s: %s", e.StatusCode, e.URL, e.Body)
}

type authResult struct {
	AccessToken string `json:"AccessToken"`
	User        struct {
		ID string `json:"Id"`
	} `json:"User"`
}

type itemList struct {
	Items            []itemDTO
	TotalRecordCount int
	TotalKnown       bool
}

type metadataTransportError struct{ Err error }

func (e metadataTransportError) Error() string { return e.Err.Error() }
func (e metadataTransportError) Unwrap() error { return e.Err }

type metadataDecodeError struct{ Err error }

func (e metadataDecodeError) Error() string { return e.Err.Error() }
func (e metadataDecodeError) Unwrap() error { return e.Err }

type itemDTO struct {
	ID                string            `json:"Id"`
	Name              string            `json:"Name"`
	Type              string            `json:"Type"`
	ImageTags         map[string]string `json:"ImageTags"`
	BackdropImageTags []string          `json:"BackdropImageTags"`
	People            []personDTO       `json:"People"`
}

type personDTO struct {
	ID              string `json:"Id"`
	Name            string `json:"Name"`
	Type            string `json:"Type"`
	PrimaryImageTag string `json:"PrimaryImageTag"`
}

type imageSource struct {
	ItemID     string `json:"item_id"`
	ItemName   string `json:"item_name"`
	ItemType   string `json:"item_type"`
	ImageType  string `json:"image_type"`
	ImageIndex int    `json:"image_index"`
	Tag        string `json:"tag"`
	SourceKind string `json:"source_kind"`
}

type warmURL struct {
	DBID     int64       `json:"-"`
	Source   imageSource `json:"source"`
	Path     string      `json:"path"`
	Variant  string      `json:"variant"`
	MaxWidth int         `json:"max_width,omitempty"`
	Quality  int         `json:"quality,omitempty"`
}

type warmResult struct {
	URLPath       string `json:"url_path"`
	Variant       string `json:"variant"`
	ImageType     string `json:"image_type"`
	HTTPStatus    int    `json:"http_status"`
	CFCacheStatus string `json:"cf_cache_status"`
	Age           string `json:"age"`
	CacheControl  string `json:"cache_control"`
	ContentType   string `json:"content_type"`
	Bytes         int64  `json:"bytes"`
	DurationMS    int64  `json:"duration_ms"`
	Error         string `json:"error,omitempty"`
}

type report struct {
	StartedAt    time.Time      `json:"started_at"`
	FinishedAt   time.Time      `json:"finished_at"`
	DryRun       bool           `json:"dry_run"`
	Sources      int            `json:"sources"`
	URLsPlanned  int            `json:"urls_planned"`
	URLsSelected int            `json:"urls_selected"`
	HTTPStatus   map[string]int `json:"http_status"`
	CFStatus     map[string]int `json:"cf_status"`
	ByVariant    map[string]int `json:"by_variant"`
	ByImageType  map[string]int `json:"by_image_type"`
	Failures     []warmResult   `json:"failures,omitempty"`
}

type runState struct {
	cfg              config
	db               *sql.DB
	dbMu             sync.Mutex
	jobs             chan warmURL
	results          chan warmResult
	workersDone      chan struct{}
	reportMu         sync.Mutex
	report           report
	seenSources      map[string]bool
	seenURLs         map[string]bool
	queuedURLs       map[string]bool
	queueMu          sync.Mutex
	selected         int
	runErr           error
	errMu            sync.Mutex
	stopOnce         sync.Once
	workerWG         sync.WaitGroup
	progressMu       sync.Mutex
	progress         progressState
	progressOut      io.Writer
	progressStop     chan struct{}
	progressStopOnce sync.Once
	progressWG       sync.WaitGroup
}

type progressState struct {
	MetadataEndpoint string
	MetadataScanned  int
	MetadataTotal    int
	MetadataKnown    bool
	MetadataPageSize int
	DiscoveryDone    bool
	PlanningDone     bool
	Completed        int
	Succeeded        int
	Failed           int
	Bytes            int64
	Active           int
}

type progressSnapshot struct {
	StartedAt        time.Time
	DryRun           bool
	Sources          int
	URLsPlanned      int
	URLsSelected     int
	QueueDepth       int
	MetadataEndpoint string
	MetadataScanned  int
	MetadataTotal    int
	MetadataKnown    bool
	MetadataPageSize int
	DiscoveryDone    bool
	PlanningDone     bool
	Completed        int
	Succeeded        int
	Failed           int
	Bytes            int64
	Active           int
}

type metadataProgress struct {
	Endpoint   string
	Scanned    int
	Total      int
	TotalKnown bool
	PageSize   int
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "imagewarmer: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: cfg.Timeout}
	started := time.Now().UTC()

	auth, err := login(ctx, client, cfg)
	if err != nil {
		return err
	}
	defer logout(context.Background(), client, cfg.GatewayURL, auth.AccessToken)

	db, err := openStateDB(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	state := newRunState(cfg, db, started)
	if err := state.start(workCtx, client, auth); err != nil {
		return err
	}
	taskCount := 1
	taskResults := make(chan error, 2)
	go func() {
		defer state.markDiscoveryDone()
		taskResults <- discoverSources(workCtx, client, cfg, auth, state.handleSources, state.updateMetadataProgress)
	}()
	if !cfg.DryRun {
		taskCount++
		go func() {
			taskResults <- state.enqueueExisting(workCtx, cfg.RefreshDone)
		}()
	}
	var runErr error
	for i := 0; i < taskCount; i++ {
		if err := <-taskResults; err != nil && runErr == nil {
			runErr = err
			cancelWork()
		}
	}
	state.markPlanningDone()
	if err := state.finish(); runErr == nil {
		runErr = err
	}
	rep := state.snapshotReport()
	rep.FinishedAt = time.Now().UTC()
	if cfg.ReportPath != "" {
		if err := writeReport(cfg.ReportPath, rep); err != nil {
			if runErr == nil {
				runErr = err
			}
		}
	}
	printSummary(rep)
	return runErr
}

func newRunState(cfg config, db *sql.DB, started time.Time) *runState {
	return &runState{
		cfg:          cfg,
		db:           db,
		jobs:         make(chan warmURL, max(1, cfg.Concurrency*2)),
		results:      make(chan warmResult, max(1, cfg.Concurrency*2)),
		workersDone:  make(chan struct{}),
		report:       report{StartedAt: started, DryRun: cfg.DryRun, HTTPStatus: map[string]int{}, CFStatus: map[string]int{}, ByVariant: map[string]int{}, ByImageType: map[string]int{}},
		seenSources:  map[string]bool{},
		seenURLs:     map[string]bool{},
		queuedURLs:   map[string]bool{},
		progressOut:  os.Stderr,
		progressStop: make(chan struct{}),
	}
}

func (s *runState) start(ctx context.Context, client *http.Client, auth authResult) error {
	s.startProgressReporter()
	if s.cfg.DryRun {
		close(s.workersDone)
		return nil
	}
	for i := 0; i < s.cfg.Concurrency; i++ {
		s.workerWG.Add(1)
		go func() {
			defer s.workerWG.Done()
			for u := range s.jobs {
				s.workerStarted()
				result := warmOne(ctx, client, s.cfg, auth, u)
				s.workerFinished()
				s.results <- result
			}
		}()
	}
	go func() {
		s.workerWG.Wait()
		close(s.results)
	}()
	go func() {
		defer close(s.workersDone)
		for result := range s.results {
			s.recordProgressResult(result)
			s.dbMu.Lock()
			err := updateWarmResult(ctx, s.db, result)
			s.dbMu.Unlock()
			if err != nil {
				s.setError(err)
				continue
			}
			s.reportMu.Lock()
			applyResult(&s.report, result)
			s.reportMu.Unlock()
		}
	}()
	return nil
}

func (s *runState) enqueueExisting(ctx context.Context, refreshDone bool) error {
	const batchSize = 1000
	var afterID int64
	for {
		remaining := s.remainingLimit()
		if remaining < 0 {
			return nil
		}
		limit := batchSize
		if remaining > 0 && remaining < limit {
			limit = remaining
		}
		s.dbMu.Lock()
		urls, err := selectWarmURLsAfter(ctx, s.db, refreshDone, afterID, limit)
		s.dbMu.Unlock()
		if err != nil {
			return err
		}
		if len(urls) == 0 {
			return nil
		}
		afterID = urls[len(urls)-1].DBID
		if err := s.enqueue(ctx, urls); err != nil {
			return err
		}
		if len(urls) < limit {
			return nil
		}
	}
}

func (s *runState) handleSources(ctx context.Context, sources []imageSource) error {
	uniqueSources := []imageSource{}
	for _, source := range sources {
		key := sourceKey(source)
		if s.seenSources[key] {
			continue
		}
		s.seenSources[key] = true
		uniqueSources = append(uniqueSources, source)
	}
	if len(uniqueSources) == 0 {
		return nil
	}

	urls := planWarmURLs(uniqueSources, s.cfg.Widths, s.cfg.Quality)
	uniqueURLs := make([]warmURL, 0, len(urls))
	for _, u := range urls {
		if s.seenURLs[u.Path] {
			continue
		}
		s.seenURLs[u.Path] = true
		uniqueURLs = append(uniqueURLs, u)
	}
	if len(uniqueURLs) == 0 {
		return nil
	}

	s.dbMu.Lock()
	err := storePlan(ctx, s.db, uniqueURLs)
	var toWarm []warmURL
	if err == nil {
		toWarm, err = selectWarmableFromList(ctx, s.db, uniqueURLs, s.cfg.RefreshDone, s.remainingLimit())
	}
	s.dbMu.Unlock()
	if err != nil {
		return err
	}

	s.reportMu.Lock()
	s.report.Sources += len(uniqueSources)
	s.report.URLsPlanned += len(uniqueURLs)
	s.reportMu.Unlock()
	return s.enqueue(ctx, toWarm)
}

func (s *runState) enqueue(ctx context.Context, urls []warmURL) error {
	for _, u := range urls {
		s.queueMu.Lock()
		if s.queuedURLs[u.Path] {
			s.queueMu.Unlock()
			continue
		}
		s.queuedURLs[u.Path] = true
		s.queueMu.Unlock()
		if !s.reserveSelection() {
			return nil
		}
		if s.cfg.DryRun {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case s.jobs <- u:
		}
	}
	return s.err()
}

func (s *runState) reserveSelection() bool {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	if s.cfg.LimitURLs > 0 && s.selected >= s.cfg.LimitURLs {
		return false
	}
	s.selected++
	s.report.URLsSelected++
	return true
}

func (s *runState) remainingLimit() int {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	if s.cfg.LimitURLs <= 0 {
		return 0
	}
	remaining := s.cfg.LimitURLs - s.selected
	if remaining <= 0 {
		return -1
	}
	return remaining
}

func (s *runState) finish() error {
	s.stop()
	s.stopProgressReporter()
	return s.err()
}

func (s *runState) stop() {
	if s.cfg.DryRun {
		return
	}
	s.stopOnce.Do(func() {
		close(s.jobs)
		<-s.workersDone
	})
}

func (s *runState) snapshotReport() report {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	return s.report
}

func (s *runState) setError(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.runErr == nil {
		s.runErr = err
	}
}

func (s *runState) err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.runErr
}

func (s *runState) startProgressReporter() {
	if s.cfg.ProgressInterval <= 0 || s.progressOut == nil {
		return
	}
	s.progressWG.Add(1)
	go func() {
		defer s.progressWG.Done()
		ticker := time.NewTicker(s.cfg.ProgressInterval)
		defer ticker.Stop()
		previousAt := time.Now()
		previous := s.progressSnapshot()
		for {
			select {
			case now := <-ticker.C:
				current := s.progressSnapshot()
				fmt.Fprintln(s.progressOut, formatProgressLine(current, now, previous, previousAt, false))
				previous = current
				previousAt = now
			case <-s.progressStop:
				now := time.Now()
				current := s.progressSnapshot()
				fmt.Fprintln(s.progressOut, formatProgressLine(current, now, previous, previousAt, true))
				return
			}
		}
	}()
}

func (s *runState) stopProgressReporter() {
	if s.cfg.ProgressInterval <= 0 || s.progressOut == nil {
		return
	}
	s.progressStopOnce.Do(func() { close(s.progressStop) })
	s.progressWG.Wait()
}

func (s *runState) workerStarted() {
	s.progressMu.Lock()
	s.progress.Active++
	s.progressMu.Unlock()
}

func (s *runState) workerFinished() {
	s.progressMu.Lock()
	s.progress.Active--
	s.progressMu.Unlock()
}

func (s *runState) recordProgressResult(result warmResult) {
	s.progressMu.Lock()
	s.progress.Completed++
	s.progress.Bytes += result.Bytes
	if warmResultSucceeded(result) {
		s.progress.Succeeded++
	} else {
		s.progress.Failed++
	}
	s.progressMu.Unlock()
}

func (s *runState) updateMetadataProgress(progress metadataProgress) {
	s.progressMu.Lock()
	s.progress.MetadataEndpoint = progress.Endpoint
	s.progress.MetadataScanned = progress.Scanned
	s.progress.MetadataTotal = progress.Total
	s.progress.MetadataKnown = progress.TotalKnown
	s.progress.MetadataPageSize = progress.PageSize
	s.progressMu.Unlock()
}

func (s *runState) markDiscoveryDone() {
	s.progressMu.Lock()
	s.progress.DiscoveryDone = true
	s.progressMu.Unlock()
}

func (s *runState) markPlanningDone() {
	s.progressMu.Lock()
	s.progress.PlanningDone = true
	s.progressMu.Unlock()
}

func (s *runState) progressSnapshot() progressSnapshot {
	s.reportMu.Lock()
	snapshot := progressSnapshot{
		StartedAt:    s.report.StartedAt,
		DryRun:       s.report.DryRun,
		Sources:      s.report.Sources,
		URLsPlanned:  s.report.URLsPlanned,
		URLsSelected: s.report.URLsSelected,
		QueueDepth:   len(s.jobs),
	}
	s.reportMu.Unlock()
	s.progressMu.Lock()
	snapshot.MetadataEndpoint = s.progress.MetadataEndpoint
	snapshot.MetadataScanned = s.progress.MetadataScanned
	snapshot.MetadataTotal = s.progress.MetadataTotal
	snapshot.MetadataKnown = s.progress.MetadataKnown
	snapshot.MetadataPageSize = s.progress.MetadataPageSize
	snapshot.DiscoveryDone = s.progress.DiscoveryDone
	snapshot.PlanningDone = s.progress.PlanningDone
	snapshot.Completed = s.progress.Completed
	snapshot.Succeeded = s.progress.Succeeded
	snapshot.Failed = s.progress.Failed
	snapshot.Bytes = s.progress.Bytes
	snapshot.Active = s.progress.Active
	s.progressMu.Unlock()
	return snapshot
}

func formatProgressLine(current progressSnapshot, now time.Time, previous progressSnapshot, previousAt time.Time, final bool) string {
	elapsed := max(now.Sub(current.StartedAt), 0)
	window := max(now.Sub(previousAt), time.Nanosecond)
	deltaCompleted := max(current.Completed-previous.Completed, 0)
	deltaBytes := max(current.Bytes-previous.Bytes, 0)
	rate := float64(deltaCompleted) / window.Seconds()
	averageRate := 0.0
	if elapsed > 0 {
		averageRate = float64(current.Completed) / elapsed.Seconds()
	}
	bandwidth := float64(deltaBytes) / window.Seconds()
	phase := "discovering"
	if current.DryRun {
		phase = "dry-run"
	} else if current.PlanningDone {
		phase = "warming"
	} else if current.DiscoveryDone {
		phase = "scheduling"
	}
	if current.PlanningDone && (current.DryRun || current.Completed >= current.URLsSelected) {
		phase = "complete"
	}
	metadataTotal := "?"
	if current.MetadataKnown {
		metadataTotal = strconv.Itoa(current.MetadataTotal)
	}
	endpoint := current.MetadataEndpoint
	if endpoint == "" {
		endpoint = "-"
	}
	eta := "discovering"
	if current.PlanningDone {
		remaining := max(current.URLsSelected-current.Completed, 0)
		etaRate := rate
		if etaRate <= 0 {
			etaRate = averageRate
		}
		switch {
		case current.DryRun || remaining == 0:
			eta = "0s"
		case etaRate > 0:
			eta = formatProgressDuration(time.Duration(float64(time.Second) * float64(remaining) / etaRate))
		default:
			eta = "unknown"
		}
	}
	return fmt.Sprintf("progress final=%t elapsed=%s phase=%s endpoint=%s metadata=%d/%s page=%d sources=%d planned=%d selected=%d completed=%d ok=%d failed=%d active=%d queue=%d rate=%.1f_img/s avg=%.1f_img/s bandwidth=%s/s eta=%s", final, formatProgressDuration(elapsed), phase, endpoint, current.MetadataScanned, metadataTotal, current.MetadataPageSize, current.Sources, current.URLsPlanned, current.URLsSelected, current.Completed, current.Succeeded, current.Failed, current.Active, current.QueueDepth, rate, averageRate, formatProgressBytes(bandwidth), eta)
}

func formatProgressDuration(duration time.Duration) string {
	if duration < time.Second {
		return "0s"
	}
	return duration.Truncate(time.Second).String()
}

func formatProgressBytes(bytesPerSecond float64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case bytesPerSecond >= gib:
		return fmt.Sprintf("%.1f_GiB", bytesPerSecond/gib)
	case bytesPerSecond >= mib:
		return fmt.Sprintf("%.1f_MiB", bytesPerSecond/mib)
	case bytesPerSecond >= kib:
		return fmt.Sprintf("%.1f_KiB", bytesPerSecond/kib)
	default:
		return fmt.Sprintf("%.0f_B", bytesPerSecond)
	}
}

func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("imagewarmer", flag.ContinueOnError)
	var widths string
	var timeout time.Duration
	cfg := config{Quality: 90, Concurrency: 8, PageSize: 500, MinPageSize: 25, DBPath: "imagewarmer.sqlite", ReportPath: "warm-report.json", IncludeItemTypes: defaultIncludeItemTypes, IncludeNames: true, Timeout: 30 * time.Second, ProgressInterval: 5 * time.Second}
	fs.StringVar(&cfg.GatewayURL, "gateway-url", "", "gateway base URL, for example https://emby.xvv.net/emby")
	fs.StringVar(&cfg.CDNURL, "cdn-url", "", "CDN base URL, for example https://emby-cf.xvv.net/emby")
	fs.StringVar(&cfg.Username, "username", "", "gateway username")
	fs.StringVar(&cfg.Password, "password", "", "gateway password")
	fs.StringVar(&widths, "widths", "320,400,640,720,800,1200,2000", "comma-separated maxWidth variants")
	fs.IntVar(&cfg.Quality, "quality", 90, "quality for maxWidth variants")
	fs.IntVar(&cfg.Concurrency, "concurrency", 8, "parallel image requests")
	fs.IntVar(&cfg.PageSize, "page-size", 500, "metadata page size")
	fs.IntVar(&cfg.MinPageSize, "min-page-size", 25, "minimum metadata page size after 502/504 fallback")
	fs.IntVar(&cfg.LimitItems, "limit-items", 0, "limit discovered media items")
	fs.IntVar(&cfg.LimitNames, "limit-names", 0, "limit discovered records per named endpoint")
	fs.IntVar(&cfg.LimitURLs, "limit-urls", 0, "limit planned/warmed URLs")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "SQLite state database path")
	fs.StringVar(&cfg.ReportPath, "report", cfg.ReportPath, "JSON report path")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "discover and plan without warming")
	fs.StringVar(&cfg.IncludeItemTypes, "include-item-types", cfg.IncludeItemTypes, "comma-separated IncludeItemTypes for media scan")
	fs.BoolVar(&cfg.IncludeNames, "include-names", true, "also scan Persons, Artists, AlbumArtists, Studios")
	fs.BoolVar(&cfg.RefreshDone, "refresh-done", false, "warm URLs even if previously done")
	fs.DurationVar(&timeout, "timeout", cfg.Timeout, "HTTP request timeout")
	fs.IntVar(&cfg.MetadataRetries, "metadata-retries", 4, "retries for transient metadata request failures")
	fs.DurationVar(&cfg.RetryDelay, "retry-delay", time.Second, "initial retry delay for transient metadata failures")
	fs.DurationVar(&cfg.ProgressInterval, "progress-interval", cfg.ProgressInterval, "progress log interval; 0 disables progress output")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if timeout > 0 {
		cfg.Timeout = timeout
	}
	if cfg.GatewayURL == "" || cfg.CDNURL == "" || cfg.Username == "" || cfg.Password == "" {
		return cfg, errors.New("--gateway-url, --cdn-url, --username and --password are required")
	}
	parsed, err := parseWidths(widths)
	if err != nil {
		return cfg, err
	}
	cfg.Widths = parsed
	cfg.GatewayURL = strings.TrimRight(cfg.GatewayURL, "/")
	cfg.CDNURL = strings.TrimRight(cfg.CDNURL, "/")
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}
	if cfg.MinPageSize <= 0 {
		cfg.MinPageSize = 1
	}
	if cfg.MinPageSize > cfg.PageSize {
		cfg.MinPageSize = cfg.PageSize
	}
	if cfg.MetadataRetries < 0 {
		cfg.MetadataRetries = 0
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = time.Second
	}
	if cfg.ProgressInterval < 0 {
		cfg.ProgressInterval = 0
	}
	return cfg, nil
}

func parseWidths(value string) ([]int, error) {
	parts := strings.Split(value, ",")
	seen := map[int]bool{}
	widths := []int{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		width, err := strconv.Atoi(part)
		if err != nil || width <= 0 {
			return nil, fmt.Errorf("invalid width %q", part)
		}
		if !seen[width] {
			seen[width] = true
			widths = append(widths, width)
		}
	}
	sort.Ints(widths)
	if len(widths) == 0 {
		return nil, errors.New("at least one width is required")
	}
	return widths, nil
}

func login(ctx context.Context, client *http.Client, cfg config) (authResult, error) {
	body := strings.NewReader(fmt.Sprintf(`{"Username":%q,"Pw":%q}`, cfg.Username, cfg.Password))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.GatewayURL+"/Users/AuthenticateByName", body)
	if err != nil {
		return authResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Authorization", `Emby Client="ImageWarmer", Device="CLI", DeviceId="image-warmer", Version="1"`)
	resp, err := client.Do(req)
	if err != nil {
		return authResult{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return authResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return authResult{}, fmt.Errorf("login status %d: %s", resp.StatusCode, string(data))
	}
	var auth authResult
	if err := json.Unmarshal(data, &auth); err != nil {
		return authResult{}, err
	}
	if auth.AccessToken == "" || auth.User.ID == "" {
		return authResult{}, errors.New("login response missing token or user id")
	}
	return auth, nil
}

func logout(ctx context.Context, client *http.Client, gatewayURL, token string) {
	if token == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(gatewayURL, "/")+"/Sessions/Logout", nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Emby-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

func discoverSources(ctx context.Context, client *http.Client, cfg config, auth authResult, handle func(context.Context, []imageSource) error, progress func(metadataProgress)) error {
	start := 0
	pageSize := cfg.PageSize
	previousPage := ""
	for {
		if cfg.LimitItems > 0 && start >= cfg.LimitItems {
			break
		}
		limit := pageSize
		if cfg.LimitItems > 0 && start+limit > cfg.LimitItems {
			limit = cfg.LimitItems - start
		}
		q := url.Values{}
		q.Set("Recursive", "true")
		q.Set("IncludeItemTypes", cfg.IncludeItemTypes)
		q.Set("Fields", "ImageTags,BackdropImageTags,People")
		page, usedLimit, err := getItemListPage(ctx, client, cfg, cfg.GatewayURL+"/Users/"+url.PathEscape(auth.User.ID)+"/Items", q, start, limit, auth.AccessToken)
		if err != nil {
			return err
		}
		if usedLimit < pageSize {
			pageSize = usedLimit
		}
		fingerprint := itemPageFingerprint(page.Items)
		if len(page.Items) > 0 && fingerprint == previousPage {
			return fmt.Errorf("metadata endpoint repeated page at StartIndex=%d", start)
		}
		previousPage = fingerprint
		if progress != nil {
			progress(metadataProgress{Endpoint: "Items", Scanned: start + len(page.Items), Total: page.TotalRecordCount, TotalKnown: page.TotalKnown, PageSize: usedLimit})
		}
		if err := handle(ctx, sourcesFromItems(page.Items, "item")); err != nil {
			return err
		}
		start += len(page.Items)
		if metadataPageComplete(page, usedLimit, start) || (cfg.LimitItems > 0 && start >= cfg.LimitItems) {
			break
		}
	}
	if cfg.IncludeNames {
		for _, endpoint := range []string{"/Persons", "/Artists", "/Artists/AlbumArtists", "/Studios"} {
			err := discoverNamedSources(ctx, client, cfg, auth, endpoint, handle, progress)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func discoverNamedSources(ctx context.Context, client *http.Client, cfg config, auth authResult, endpoint string, handle func(context.Context, []imageSource) error, progress func(metadataProgress)) error {
	start := 0
	pageSize := cfg.PageSize
	previousPage := ""
	for {
		if cfg.LimitNames > 0 && start >= cfg.LimitNames {
			break
		}
		limit := pageSize
		if cfg.LimitNames > 0 && start+limit > cfg.LimitNames {
			limit = cfg.LimitNames - start
		}
		q := url.Values{}
		q.Set("Fields", "ImageTags")
		page, usedLimit, err := getItemListPage(ctx, client, cfg, cfg.GatewayURL+endpoint, q, start, limit, auth.AccessToken)
		if err != nil {
			return err
		}
		if usedLimit < pageSize {
			pageSize = usedLimit
		}
		fingerprint := itemPageFingerprint(page.Items)
		if len(page.Items) > 0 && fingerprint == previousPage {
			return fmt.Errorf("metadata endpoint %s repeated page at StartIndex=%d", endpoint, start)
		}
		previousPage = fingerprint
		if progress != nil {
			progress(metadataProgress{Endpoint: strings.TrimPrefix(endpoint, "/"), Scanned: start + len(page.Items), Total: page.TotalRecordCount, TotalKnown: page.TotalKnown, PageSize: usedLimit})
		}
		kind := strings.Trim(strings.ToLower(strings.ReplaceAll(endpoint, "/", "_")), "_")
		if err := handle(ctx, sourcesFromItems(page.Items, kind)); err != nil {
			return err
		}
		start += len(page.Items)
		if metadataPageComplete(page, usedLimit, start) || (cfg.LimitNames > 0 && start >= cfg.LimitNames) {
			break
		}
	}
	return nil
}

func metadataPageComplete(page itemList, requestedLimit, nextStart int) bool {
	if len(page.Items) == 0 {
		return true
	}
	if page.TotalKnown {
		return nextStart >= page.TotalRecordCount
	}
	return len(page.Items) < requestedLimit
}

func itemPageFingerprint(items []itemDTO) string {
	var fingerprint strings.Builder
	for _, item := range items {
		fingerprint.WriteString(item.ID)
		fingerprint.WriteByte('|')
		fingerprint.WriteString(item.Name)
		fingerprint.WriteByte('\n')
	}
	return fingerprint.String()
}

func getItemListPage(ctx context.Context, client *http.Client, cfg config, baseURL string, query url.Values, start, limit int, token string) (itemList, int, error) {
	currentLimit := limit
	minimum := min(cfg.MinPageSize, limit)
	if minimum <= 0 {
		minimum = 1
	}
	for {
		requestURL := itemListURL(baseURL, query, start, currentLimit)
		page, err := getItemListWithRetry(ctx, client, requestURL, token, cfg.MetadataRetries, cfg.RetryDelay)
		if err == nil {
			return page, currentLimit, nil
		}
		if !shouldReduceMetadataPage(err) || currentLimit <= minimum {
			return itemList{}, currentLimit, err
		}
		nextLimit := max(minimum, currentLimit/2)
		if nextLimit == currentLimit {
			return itemList{}, currentLimit, err
		}
		currentLimit = nextLimit
	}
}

func itemListURL(baseURL string, query url.Values, start, limit int) string {
	q := url.Values{}
	for key, values := range query {
		for _, value := range values {
			q.Add(key, value)
		}
	}
	q.Set("StartIndex", strconv.Itoa(start))
	q.Set("Limit", strconv.Itoa(limit))
	return baseURL + "?" + q.Encode()
}

func getItemListWithRetry(ctx context.Context, client *http.Client, requestURL, token string, retries int, delay time.Duration) (itemList, error) {
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		page, err := getItemList(ctx, client, requestURL, token)
		if err == nil || !isRetryableMetadataError(ctx, err) || attempt == retries {
			return page, err
		}
		lastErr = err
		wait := metadataRetryWait(delay, attempt, err)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return itemList{}, ctx.Err()
		case <-timer.C:
		}
	}
	return itemList{}, lastErr
}

func getItemList(ctx context.Context, client *http.Client, requestURL, token string) (itemList, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return itemList{}, err
	}
	req.Header.Set("X-Emby-Token", token)
	req.Header.Set("X-Emby-Authorization", `Emby Client="ImageWarmer", Device="CLI", DeviceId="image-warmer", Version="1"`)
	resp, err := client.Do(req)
	if err != nil {
		return itemList{}, metadataTransportError{Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return itemList{}, metadataTransportError{Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return itemList{}, metadataStatusError{StatusCode: resp.StatusCode, URL: requestURL, Body: string(data), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return itemList{}, metadataDecodeError{Err: errors.New("metadata response body is empty")}
	}
	if trimmed[0] == '{' {
		var response struct {
			Items            []itemDTO `json:"Items"`
			TotalRecordCount *int      `json:"TotalRecordCount"`
		}
		if err := json.Unmarshal(trimmed, &response); err != nil {
			return itemList{}, metadataDecodeError{Err: err}
		}
		list := itemList{Items: response.Items}
		if response.TotalRecordCount != nil {
			list.TotalRecordCount = *response.TotalRecordCount
			list.TotalKnown = true
		}
		return list, nil
	}
	var items []itemDTO
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return itemList{}, metadataDecodeError{Err: err}
	}
	return itemList{Items: items}, nil
}

func isRetryableMetadataError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var statusErr metadataStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode >= 500
	}
	var transportErr metadataTransportError
	var decodeErr metadataDecodeError
	return errors.As(err, &transportErr) || errors.As(err, &decodeErr)
}

func shouldReduceMetadataPage(err error) bool {
	var statusErr metadataStatusError
	return errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusBadGateway || statusErr.StatusCode == http.StatusGatewayTimeout)
}

func metadataRetryWait(base time.Duration, attempt int, err error) time.Duration {
	const maximum = 30 * time.Second
	multiplier := time.Duration(1 << min(attempt, 5))
	wait := min(base*multiplier, maximum)
	var statusErr metadataStatusError
	if errors.As(err, &statusErr) && statusErr.RetryAfter > wait {
		wait = min(statusErr.RetryAfter, maximum)
	}
	jitter := wait / 5
	if jitter > 0 {
		wait = wait - jitter/2 + time.Duration(rand.Int64N(int64(jitter)+1))
	}
	return wait
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func sourcesFromItems(items []itemDTO, sourceKind string) []imageSource {
	sources := []imageSource{}
	for _, item := range items {
		for imageType, tag := range item.ImageTags {
			if strings.TrimSpace(tag) == "" || strings.TrimSpace(item.ID) == "" {
				continue
			}
			sources = append(sources, imageSource{ItemID: item.ID, ItemName: item.Name, ItemType: item.Type, ImageType: imageType, ImageIndex: -1, Tag: tag, SourceKind: sourceKind})
		}
		for i, tag := range item.BackdropImageTags {
			if strings.TrimSpace(tag) == "" || strings.TrimSpace(item.ID) == "" {
				continue
			}
			sources = append(sources, imageSource{ItemID: item.ID, ItemName: item.Name, ItemType: item.Type, ImageType: "Backdrop", ImageIndex: i, Tag: tag, SourceKind: sourceKind})
		}
		for _, person := range item.People {
			if strings.TrimSpace(person.ID) == "" || strings.TrimSpace(person.PrimaryImageTag) == "" {
				continue
			}
			sources = append(sources, imageSource{ItemID: person.ID, ItemName: person.Name, ItemType: "Person", ImageType: "Primary", ImageIndex: -1, Tag: person.PrimaryImageTag, SourceKind: "person"})
		}
	}
	return sources
}

func sourceKey(source imageSource) string {
	return strings.Join([]string{source.ItemID, source.ImageType, strconv.Itoa(source.ImageIndex), source.Tag}, "|")
}

func planWarmURLs(sources []imageSource, widths []int, quality int) []warmURL {
	seen := map[string]bool{}
	urls := []warmURL{}
	for _, source := range sources {
		if source.Tag == "" || source.ItemID == "" || source.ImageType == "" {
			continue
		}
		basePath := imagePath(source)
		add := func(variant string, maxWidth, q int, query string) {
			path := basePath + "?" + query
			if seen[path] {
				return
			}
			seen[path] = true
			urls = append(urls, warmURL{Source: source, Path: path, Variant: variant, MaxWidth: maxWidth, Quality: q})
		}
		add("tag_only", 0, 0, "tag="+url.QueryEscape(source.Tag))
		for _, width := range widths {
			query := "maxWidth=" + strconv.Itoa(width) + "&tag=" + url.QueryEscape(source.Tag) + "&quality=" + strconv.Itoa(quality)
			add("maxWidth="+strconv.Itoa(width)+"_quality="+strconv.Itoa(quality), width, quality, query)
		}
	}
	sort.Slice(urls, func(i, j int) bool { return urls[i].Path < urls[j].Path })
	return urls
}

func imagePath(source imageSource) string {
	base := "/Items/" + url.PathEscape(source.ItemID) + "/Images/" + url.PathEscape(source.ImageType)
	if source.ImageIndex >= 0 {
		base += "/" + strconv.Itoa(source.ImageIndex)
	}
	return base
}

func openStateDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	stmts := []string{
		`create table if not exists image_sources (id integer primary key autoincrement, item_id text, item_name text, item_type text, image_type text, image_index integer, tag text, source_kind text, discovered_at text, unique(item_id, image_type, image_index, tag))`,
		`create table if not exists warm_urls (id integer primary key autoincrement, item_id text, image_type text, image_index integer, tag text, source_kind text, url_path text unique, variant text, max_width integer, quality integer, status text default 'pending', attempts integer default 0, last_http_status integer, last_cf_cache_status text, last_age text, last_content_type text, last_bytes integer, last_duration_ms integer, last_error text, last_warmed_at text)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

func storePlan(ctx context.Context, db *sql.DB, urls []warmURL) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, u := range urls {
		_, err = tx.ExecContext(ctx, `insert or ignore into image_sources (item_id,item_name,item_type,image_type,image_index,tag,source_kind,discovered_at) values (?,?,?,?,?,?,?,?)`, u.Source.ItemID, u.Source.ItemName, u.Source.ItemType, u.Source.ImageType, u.Source.ImageIndex, u.Source.Tag, u.Source.SourceKind, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `insert or ignore into warm_urls (item_id,image_type,image_index,tag,source_kind,url_path,variant,max_width,quality,status) values (?,?,?,?,?,?,?,?,?,'pending')`, u.Source.ItemID, u.Source.ImageType, u.Source.ImageIndex, u.Source.Tag, u.Source.SourceKind, u.Path, u.Variant, u.MaxWidth, u.Quality)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func selectWarmURLs(ctx context.Context, db *sql.DB, refreshDone bool, limit int) ([]warmURL, error) {
	return selectWarmURLsAfter(ctx, db, refreshDone, 0, limit)
}

func selectWarmURLsAfter(ctx context.Context, db *sql.DB, refreshDone bool, afterID int64, limit int) ([]warmURL, error) {
	where := "status != 'done'"
	if refreshDone {
		where = "1=1"
	}
	query := `select id,item_id,image_type,image_index,tag,source_kind,url_path,variant,max_width,quality from warm_urls where ` + where + ` and id > ? order by id`
	args := []any{afterID}
	if limit > 0 {
		query += " limit ?"
		args = append(args, limit)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var urls []warmURL
	for rows.Next() {
		var u warmURL
		if err := rows.Scan(&u.DBID, &u.Source.ItemID, &u.Source.ImageType, &u.Source.ImageIndex, &u.Source.Tag, &u.Source.SourceKind, &u.Path, &u.Variant, &u.MaxWidth, &u.Quality); err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	return urls, rows.Err()
}

func selectWarmableFromList(ctx context.Context, db *sql.DB, urls []warmURL, refreshDone bool, limit int) ([]warmURL, error) {
	if limit < 0 {
		return nil, nil
	}
	selected := make([]warmURL, 0, len(urls))
	for _, u := range urls {
		if limit > 0 && len(selected) >= limit {
			break
		}
		var status string
		err := db.QueryRowContext(ctx, `select status from warm_urls where url_path = ?`, u.Path).Scan(&status)
		if err != nil {
			return nil, err
		}
		if refreshDone || status != "done" {
			selected = append(selected, u)
		}
	}
	return selected, nil
}

func warmAll(ctx context.Context, client *http.Client, cfg config, auth authResult, urls []warmURL) []warmResult {
	jobs := make(chan warmURL)
	results := make(chan warmResult)
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				results <- warmOne(ctx, client, cfg, auth, u)
			}
		}()
	}
	go func() {
		for _, u := range urls {
			jobs <- u
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	collected := []warmResult{}
	for result := range results {
		collected = append(collected, result)
	}
	return collected
}

func warmOne(ctx context.Context, client *http.Client, cfg config, auth authResult, u warmURL) warmResult {
	started := time.Now()
	result := warmResult{URLPath: u.Path, Variant: u.Variant, ImageType: u.Source.ImageType}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.CDNURL+u.Path, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	req.Header.Set("X-Emby-Token", auth.AccessToken)
	req.Header.Set("X-Emby-Authorization", `Emby Client="ImageWarmer", Device="CLI", DeviceId="image-warmer", Version="1"`)
	resp, err := client.Do(req)
	if err != nil {
		result.Error = err.Error()
		result.DurationMS = time.Since(started).Milliseconds()
		return result
	}
	defer resp.Body.Close()
	validator := newDownloadedImageValidator(resp.Header.Get("Content-Type"))
	bytes, readErr := io.Copy(validator, resp.Body)
	result.HTTPStatus = resp.StatusCode
	result.CFCacheStatus = resp.Header.Get("CF-Cache-Status")
	result.Age = resp.Header.Get("Age")
	result.CacheControl = resp.Header.Get("Cache-Control")
	result.ContentType = resp.Header.Get("Content-Type")
	result.Bytes = bytes
	result.DurationMS = time.Since(started).Milliseconds()
	if readErr != nil {
		result.Error = readErr.Error()
	} else if resp.StatusCode >= 200 && resp.StatusCode < 300 && !strings.HasPrefix(strings.ToLower(result.ContentType), "image/") {
		result.Error = "response content type is not an image"
	} else if resp.StatusCode >= 200 && resp.StatusCode < 300 && bytes == 0 {
		result.Error = "response image body is empty"
	} else if resp.ContentLength >= 0 && bytes != resp.ContentLength {
		result.Error = fmt.Sprintf("response body length %d does not match Content-Length %d", bytes, resp.ContentLength)
	} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.Error = validator.Validate()
	}
	return result
}

type downloadedImageValidator struct {
	mediaType string
	prefix    []byte
	tail      []byte
	total     int64
}

func newDownloadedImageValidator(contentType string) *downloadedImageValidator {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return &downloadedImageValidator{mediaType: mediaType}
}

func (v *downloadedImageValidator) Write(p []byte) (int, error) {
	v.total += int64(len(p))
	if len(v.prefix) < imageValidationTailSize {
		need := imageValidationTailSize - len(v.prefix)
		if need > len(p) {
			need = len(p)
		}
		v.prefix = append(v.prefix, p[:need]...)
	}
	combined := make([]byte, 0, len(v.tail)+len(p))
	combined = append(combined, v.tail...)
	combined = append(combined, p...)
	if len(combined) > imageValidationTailSize {
		combined = combined[len(combined)-imageValidationTailSize:]
	}
	v.tail = append(v.tail[:0], combined...)
	return len(p), nil
}

func (v *downloadedImageValidator) Validate() string {
	switch v.mediaType {
	case "image/jpeg", "image/jpg":
		if len(v.prefix) < 2 || len(v.tail) < 2 || v.prefix[0] != 0xff || v.prefix[1] != 0xd8 || v.tail[len(v.tail)-2] != 0xff || v.tail[len(v.tail)-1] != 0xd9 {
			return "response JPEG image is incomplete"
		}
	case "image/png":
		pngSignature := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
		pngIEND := []byte{0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82}
		if len(v.prefix) < len(pngSignature) || !bytes.Equal(v.prefix[:len(pngSignature)], pngSignature) || len(v.tail) != len(pngIEND) || !bytes.Equal(v.tail, pngIEND) {
			return "response PNG image is incomplete"
		}
	case "image/webp":
		if len(v.prefix) < 12 || string(v.prefix[:4]) != "RIFF" || string(v.prefix[8:12]) != "WEBP" || int64(binary.LittleEndian.Uint32(v.prefix[4:8]))+8 != v.total {
			return "response WebP image is incomplete"
		}
	}
	return ""
}

func updateWarmResult(ctx context.Context, db *sql.DB, result warmResult) error {
	status := "done"
	if !warmResultSucceeded(result) {
		status = "failed"
	}
	_, err := db.ExecContext(ctx, `update warm_urls set status=?, attempts=attempts+1, last_http_status=?, last_cf_cache_status=?, last_age=?, last_content_type=?, last_bytes=?, last_duration_ms=?, last_error=?, last_warmed_at=? where url_path=?`, status, result.HTTPStatus, result.CFCacheStatus, result.Age, result.ContentType, result.Bytes, result.DurationMS, result.Error, time.Now().UTC().Format(time.RFC3339), result.URLPath)
	return err
}

func warmResultSucceeded(result warmResult) bool {
	return result.Error == "" && result.HTTPStatus >= 200 && result.HTTPStatus < 300 && strings.HasPrefix(strings.ToLower(result.ContentType), "image/")
}

func applyResult(rep *report, result warmResult) {
	statusKey := strconv.Itoa(result.HTTPStatus)
	if result.HTTPStatus == 0 {
		statusKey = "error"
	}
	rep.HTTPStatus[statusKey]++
	cf := result.CFCacheStatus
	if cf == "" {
		cf = "none"
	}
	rep.CFStatus[cf]++
	rep.ByVariant[result.Variant]++
	rep.ByImageType[result.ImageType]++
	if result.Error != "" || result.HTTPStatus < 200 || result.HTTPStatus >= 300 {
		rep.Failures = append(rep.Failures, result)
	}
}

func writeReport(path string, rep report) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func printSummary(rep report) {
	fmt.Printf("sources=%d urls_planned=%d urls_selected=%d dry_run=%v\n", rep.Sources, rep.URLsPlanned, rep.URLsSelected, rep.DryRun)
	if !rep.DryRun {
		fmt.Printf("http_status=%v\n", rep.HTTPStatus)
		fmt.Printf("cf_status=%v\n", rep.CFStatus)
		fmt.Printf("failures=%d\n", len(rep.Failures))
	}
}
