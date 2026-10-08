// Command mock-toniebox drives a real teddyCloud as a Toniebox would, without
// the box.
//
// It exists to make docs/ideal-shim-behavior.md implementable and testable: the
// box is the only actor that both emits the RTNL frames teddyCloud turns into
// SSE and pulls the content that makes teddyCloud's ffmpeg call the shim's
// /stream. Mock it and the whole chain runs for real, with the shim untouched.
//
// The tool is deliberately not a script. Box behaviour that firmware could
// demonstrate differently is exposed as flags rather than baked in, and the
// active set is stamped into every report — a measurement is only attributable
// alongside the assumptions that produced it. See docs/mock-toniebox-design.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/toniebox"
)

const (
	rtlnMinBackoff = 1 * time.Second
	rtlnMaxBackoff = 30 * time.Second

	// settleAfterLift is how long the cycle generator waits for the last lift's
	// playback=stopped to arrive on the SSE feed. The lift is a frame write, but
	// the event the report counts is teddyCloud's reaction to it, and the
	// observer sees that on its own schedule. Without the wait, a run's
	// stopped-per-cycle invariant is short by exactly one however correct the
	// box's behaviour is.
	settleAfterLift = 1500 * time.Millisecond
)

var (
	addrFlag     = flag.String("addr", ":9095", "control HTTP listen address")
	tcFlag       = flag.String("teddycloud", "https://localhost:443", "teddyCloud API listener base URL")
	sniFlag      = flag.Bool("sni", false, "send SNI, selecting teddyCloud's TB2 certificate chain (default is TB1, no SNI)")
	firmwareFlag = flag.String("firmware", "cc3200", "box firmware profile: cc3200 or esp32")
	macFlag      = flag.String("mac", "00aabbccdd00", "12 hex character box MAC; becomes teddyCloud's overlay id")
	uidFlag      = flag.String("uid", "E00403500EEA4BF2", "figurine rUID, 16 hex characters as shown by teddyCloud")

	contentPathFlag = flag.String("content-path", "v3", "content route: v2 or v3")
	fallbackFlag    = flag.String("fallback", "on-404", "content route fallback: off, on-404 or always")
	freshnessFlag   = flag.String("freshness", "v1", "freshness-check endpoint: v1, v3 or none")
	reportAudioFlag = flag.String("report-audio-id", "taf", "audio id reported on freshness check: taf, zero, stale or omit")
	recheckFlag     = flag.Bool("recheck", true, "run a freshness check on every placement")
	rangeFlag       = flag.String("range-offset", "zero", "content request offset: zero or resume")

	speedFlag   = flag.Float64("speed", 1, "content read rate as a multiple of real time; 0 disables pacing (use /control/stall to stop reading without disconnecting)")
	rateFlag    = flag.Int64("rate", toniebox.DefaultStreamRate, "nominal payload rate in bytes per second that --speed scales")
	stallFlag   = flag.Int("stall-ms", 0, "pause the content read for N ms at the start of every download")
	dropFlag    = flag.Int64("drop-after", 0, "abort every content body after N bytes (combine with --range-offset=resume)")
	maxDownFlag = flag.Int64("max-download", 0, "stop reading after N bytes and hold the connection open")

	holdFlag     = flag.Duration("hold", 5*time.Second, "how long /control/cycle holds a figurine before lifting it")
	gapFlag      = flag.Duration("gap", 3*time.Second, "how long /control/cycle waits between lifting and the next placement")
	obsFlag      = flag.Bool("observe", true, "subscribe to teddyCloud's /api/sse feed and record events")
	sseFlag      = flag.String("sse", "", "teddyCloud web listener base URL for /api/sse; defaults to http://<teddycloud host>:80")
	doubleMSFlag = flag.Int("double-press-ms", 150, "gap between the volume frame and an ear press, inside teddyCloud's 300 ms window")
	padFlag      = flag.Int("pad-field6", 0, "extra filler bytes appended to the tag frame field6")

	levelFlag = flag.String("log-level", "info", "log level: debug, info, warn, error")
)

func main() {
	flag.Parse()

	configureLogging(*levelFlag)

	if err := run(); err != nil {
		slog.Error("mock-toniebox: exiting", "err", err)
		os.Exit(1)
	}
}

func configureLogging(level string) {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		slog.Warn("mock-toniebox: unknown log level, falling back to info", "level", level)
		parsed = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed})))
}

// mock owns every moving part: the box, the connection, the recorder, and the
// serialisation of box actions.
type mock struct {
	box        *toniebox.Box
	client     *toniebox.Client
	downloader *toniebox.Downloader
	rec        *recorder

	// mu serialises box actions. Placements are sequential by nature — one
	// figurine on the box — and interleaving a lift with a freshness check
	// would produce states no box ever reaches.
	mu sync.Mutex

	// runMu guards the RTNL connection lifecycle so that a reconnecting writer
	// and a control handler cannot race.
	runMu sync.Mutex
}

func run() error {
	uid, err := toniebox.ParseRUID(*uidFlag)
	if err != nil {
		return err
	}
	firmware, err := toniebox.ParseFirmware(*firmwareFlag)
	if err != nil {
		return err
	}
	contentPath, err := toniebox.ParseContentPath(*contentPathFlag)
	if err != nil {
		return err
	}
	fallback, err := toniebox.ParseFallback(*fallbackFlag)
	if err != nil {
		return err
	}
	freshness, err := toniebox.ParseFreshness(*freshnessFlag)
	if err != nil {
		return err
	}
	reportAudio, err := toniebox.ParseReportAudioID(*reportAudioFlag)
	if err != nil {
		return err
	}
	rangeOffset, err := toniebox.ParseRangeOffset(*rangeFlag)
	if err != nil {
		return err
	}

	identity, err := toniebox.NewIdentity(*macFlag)
	if err != nil {
		return err
	}

	cfg := toniebox.DefaultConfig(*macFlag, uid)
	cfg.Firmware = firmware
	cfg.ContentPath = contentPath
	cfg.Fallback = fallback
	cfg.Freshness = freshness
	cfg.Report = reportAudio
	cfg.Recheck = *recheckFlag
	cfg.Range = rangeOffset
	cfg.Speed = *speedFlag
	cfg.StallMS = *stallFlag
	cfg.DropAfter = *dropFlag
	cfg.MaxDownload = *maxDownFlag
	cfg.DoublePressMS = *doubleMSFlag
	cfg.PadField6 = *padFlag

	box := toniebox.NewBox(cfg)

	client := toniebox.NewClient(toniebox.ClientConfig{
		BaseURL:  *tcFlag,
		Identity: identity,
		SNI:      *sniFlag,
		Warn: func(msg string, args ...any) {
			slog.Warn(msg, args...)
		},
	})
	defer func() {
		if err := client.Close(); err != nil {
			slog.Debug("mock-toniebox: closing client", "err", err)
		}
	}()

	// --speed is the read rate as a multiple of real time and --rate is the
	// nominal real-time rate it scales. Multiplying here means the two flags stay
	// independent and both land in the assumption set that gets stamped into every
	// report. Passing only --rate would make --speed inert, which is worse than
	// not having it: a starved consumer scenario would appear to run.
	rate := *rateFlag
	if cfg.Speed <= 0 {
		// Zero means "stop reading without disconnecting", which is a different
		// condition from a fast link and is expressed by blocking the gate
		// instead of by an unbounded rate.
		rate = 0
		slog.Info("mock-toniebox: speed 0 — the box will read as fast as the source allows; use /control/stall to stop reading without disconnecting")
	} else {
		rate = int64(float64(rate) * cfg.Speed)
	}
	downloader := toniebox.NewDownloader(box, client, rate)

	m := &mock{
		box:        box,
		client:     client,
		downloader: downloader,
		rec:        newRecorder(cfg, identity.Overlay),
	}
	downloader.OnProgress = m.rec.progress

	printBanner(cfg, identity, *tcFlag)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *obsFlag {
		sseBase := sseBaseURL(*tcFlag, *sseFlag)
		if sseBase != *tcFlag {
			slog.Info("mock-toniebox: observing the web listener, which is a different port from the box API",
				"sse", sseBase, "box_api", *tcFlag)
		}
		obs := newObserver(sseBase, plainHTTPClient(), m.rec.note)
		go obs.run(ctx)
	}

	go m.writeRTNL(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/", m.handleIndex)
	mux.HandleFunc("/control/", m.handleControl)
	mux.HandleFunc("/api/sse", m.handleSSE)
	mux.HandleFunc("/control/report", m.handleReport)

	server := &http.Server{
		Addr:              *addrFlag,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Debug("mock-toniebox: control server shutdown", "err", err)
		}
	}()

	slog.Info("mock-toniebox: control surface listening", "addr", *addrFlag)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	// Render the final report on the way out, so a finished cycle run leaves an
	// artefact even if nobody asked for one.
	//
	// A failure here is reported rather than returned: the measurements have
	// already been delivered on /control/report, and losing them because the
	// working directory is read-only would be the wrong trade.
	cycles, _ := m.rec.snapshot()
	const reportPath = "mock-toniebox-report.json"
	if err := os.WriteFile(reportPath, writeReport(m.rec.report(cycles)), 0o644); err != nil {
		slog.Warn("mock-toniebox: could not write the report; the measurements are still on /control/report",
			"path", reportPath, "err", err)
	} else {
		slog.Info("mock-toniebox: report written", "path", reportPath)
	}

	return nil
}

// sseBaseURL resolves where the SSE feed lives. /api/sse is registered SERTY_WEB
// (src/server.c route table) and the web listener is a separate port from the
// HTTPS API listener (src/server.c:915 and :935), so it cannot share the box's
// base URL. The default assumes the stock ports and the same host.
func sseBaseURL(tcBase, override string) string {
	if override != "" {
		return strings.TrimSuffix(override, "/")
	}
	u, err := url.Parse(tcBase)
	if err != nil || u.Hostname() == "" {
		return "http://localhost:80"
	}
	return "http://" + u.Hostname() + ":80"
}

// plainHTTPClient is deliberately bare: the web listener has no identity gate
// (src/server.c:314 only runs when api_access_only is set), so sending the box
// certificate there would be both useless and misleading.
func plainHTTPClient() *http.Client {
	return &http.Client{}
}

func printBanner(cfg toniebox.Config, identity *toniebox.Identity, baseURL string) {
	lines := []string{
		"mock-toniebox: box assumptions in effect for this run",
		"  teddycloud: " + baseURL,
	}
	for _, pair := range cfg.Assumptions() {
		lines = append(lines, fmt.Sprintf("  %-20s %s", pair[0]+":", pair[1]))
	}
	lines = append(lines, fmt.Sprintf("  %-20s %s", "issuer CN:", "TeddyCloud (gate in src/server.c:320-321)"))
	slog.Info(strings.Join(lines, "\n"), "overlay", identity.Overlay)
}

// send writes frames to the telemetry connection, redialling once if it has
// dropped. A box that lost its connection retries; the mock does the same, so a
// teddyCloud restart is an observed event rather than a fatal error.
func (m *mock) send(frames ...[]byte) error {
	m.runMu.Lock()
	defer m.runMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := m.client.RTNLConn(ctx); err != nil {
		return err
	}
	if err := m.client.WriteRTNL(frames...); err == nil {
		return nil
	} else {
		slog.Warn("mock-toniebox: RTNL write failed, redialling", "err", err)
	}

	m.client.DisconnectRTNL()
	if _, err := m.client.RTNLConn(ctx); err != nil {
		return fmt.Errorf("mock-toniebox: redialling RTNL: %w", err)
	}
	return m.client.WriteRTNL(frames...)
}

// writeRTNL keeps the telemetry connection warm until ctx is cancelled. It is a
// goroutine rather than a dial-on-demand so that a connection that has gone away
// silently is noticed and re-established before the next control action needs
// it.
func (m *mock) writeRTNL(ctx context.Context) {
	backoff := rtlnMinBackoff
	for ctx.Err() == nil {
		m.runMu.Lock()
		_, err := m.client.RTNLConn(ctx)
		m.runMu.Unlock()

		if err == nil {
			backoff = rtlnMinBackoff
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, rtlnMaxBackoff)
	}
}

func (m *mock) place(ctx context.Context, uid uint64, hold time.Duration) (toniebox.PlacementResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	frames, err := m.box.TagFrames(toniebox.TagValid, uid)
	if err != nil {
		return toniebox.PlacementResult{}, err
	}
	if err := m.send(frames...); err != nil {
		return toniebox.PlacementResult{}, err
	}

	holdCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		res toniebox.PlacementResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := m.downloader.Place(holdCtx, uid)
		done <- outcome{res, err}
	}()

	start := time.Now()
	timer := time.NewTimer(hold)
	defer timer.Stop()

	select {
	case out := <-done:
		// A small body finishes long before the figurine comes off the box. The
		// hold is a phase of the cycle, not a timeout on the download, so keep
		// waiting: otherwise --hold only ever bounds a slow body and a fast one
		// collapses the cadence the load generator is meant to hold.
		if remaining := hold - time.Since(start); remaining > 0 && !sleepCtx(ctx, remaining) {
			return toniebox.PlacementResult{}, ctx.Err()
		}
		return out.res, out.err
	case <-timer.C:
		cancel()
		out := <-done
		// Cancellation on hold expiry is how the box lifts the figurine, not a
		// failure of the download.
		if out.err != nil && (errors.Is(out.err, context.Canceled) || errors.Is(out.err, toniebox.ErrStalled)) {
			out.err = nil
		}
		return out.res, out.err
	case <-ctx.Done():
		cancel()
		<-done
		return toniebox.PlacementResult{}, ctx.Err()
	}
}

func (m *mock) lift(uid uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	stopFrames, err := m.box.PlaybackStopFrames(12, -3)
	if err != nil {
		return err
	}
	removeFrames, err := m.box.TagFrames(toniebox.TagRemoved, uid)
	if err != nil {
		return err
	}
	return m.send(append(stopFrames, removeFrames...)...)
}

// runCycle is the load generator: N placements at a fixed cadence, with the
// measurements for each appended to the recorder. It replaces the five
// operator-driven lift/place cycles with a stopwatch from plan_debug_sound.md
// with a repeatable run and a machine-checked table.
func (m *mock) runCycle(ctx context.Context, count int, gap, hold time.Duration) error {
	slog.Info("mock-toniebox: starting cycle run", "count", count, "hold", hold, "gap", gap)

	for i := 1; i <= count; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		uid, _ := m.box.CurrentUID()
		if uid == 0 {
			uid = m.box.Config().UID
		}

		m.rec.begin(i, uid)
		if _, err := m.place(ctx, uid, *holdFlag); err != nil {
			slog.Warn("mock-toniebox: placement failed", "cycle", i, "err", err)
		}
		_ = m.rec.finish()

		// Every cycle lifts, including the last one: an unlifted figurine
		// leaves teddyCloud without the playback=stopped that the report
		// counts as one of its invariants.
		//
		// The lift has to land *after* finish(), which it does. Recording the
		// stop inside the cycle would attribute it to the placement that
		// follows, and the recorded stop timestamps would then be off by a whole
		// cycle in the inter-cycle gap measurement.
		if err := m.lift(uid); err != nil {
			slog.Warn("mock-toniebox: lift failed", "cycle", i, "err", err)
		}

		if i == count {
			// The final lift's playback=stopped is asynchronous: teddyCloud
			// publishes it once it has processed the frame, which happens after
			// this function returns. Give it a moment so the last cycle's stop
			// is counted, otherwise the invariant always fails by one.
			if !sleepCtx(ctx, settleAfterLift) {
				return ctx.Err()
			}
			break
		}
		if !sleepCtx(ctx, gap) {
			return ctx.Err()
		}
	}

	cycles, _ := m.rec.snapshot()
	slog.Info("mock-toniebox: cycle run complete", "cycles", len(cycles))
	return nil
}

func parseCount(r *http.Request) int {
	n := 0
	if v := r.URL.Query().Get("count"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &n)
	}
	if n <= 0 {
		return 1
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("mock-toniebox: encoding response", "err", err)
	}
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
