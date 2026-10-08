package toniebox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultStreamRate is the nominal payload rate the read pacer assumes for real
// time. The mock cannot know the bitrate teddyCloud's ffmpeg will produce, and
// getting it wrong only scales every --speed value uniformly, so it is an
// assumption rather than a measurement — override it with --rate.
const DefaultStreamRate = 24 * 1024

// ErrStalled is returned when a gated download is cancelled while suspended.
var ErrStalled = errors.New("toniebox: download cancelled while stalled")

// Gate suspends and resumes a download in place.
//
// This is the distinction the mock needs to be able to draw: a box that stops
// reading without disconnecting leaves teddyCloud with an open connection and a
// body that has stopped advancing, which is a different situation from one that
// closed the socket and comes back with a Range.
type Gate struct {
	mu     sync.Mutex
	open   bool
	closed chan struct{}
}

// NewGate returns an open gate.
func NewGate() *Gate {
	g := &Gate{open: true, closed: make(chan struct{})}
	close(g.closed)
	return g
}

// Block suspends the gate. Calling it on an already suspended gate is a no-op.
func (g *Gate) Block() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		return
	}
	g.open = false
	g.closed = make(chan struct{})
}

// Resume releases the gate.
func (g *Gate) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open {
		return
	}
	g.open = true
	close(g.closed)
}

// Suspended reports whether the gate is currently closed.
func (g *Gate) Suspended() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.open
}

// Wait blocks until the gate is open or ctx is done.
func (g *Gate) Wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.closed
	open := g.open
	g.mu.Unlock()

	if open {
		return nil
	}

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrStalled, ctx.Err())
	}
}

// ContentItem is one entry of the /v3/content-meta chapter list.
type ContentItem struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Auth     string `json:"auth"`
	FileSize int64  `json:"fileSize"`
}

// ContentMeta is the /v3/content-meta reply. Version is the TAF audio id, which
// is how a box learns the id of what it is about to download
// (src/handler_cloud.c:1413-1483).
type ContentMeta struct {
	Version        uint64        `json:"version"`
	ContentType    string        `json:"contentType"`
	TonieType      string        `json:"tonieType"`
	ResumeBehavior string        `json:"resumeBehavior"`
	TonieSalesID   string        `json:"tonieSalesId"`
	Content        []ContentItem `json:"content"`
}

// statusError reports an HTTP status a route answered with, so that the
// fallback decision can be made on the status rather than on the presence of an
// error at all.
//
// This distinction is load-bearing. A 404 on /v3/content-meta means the route is
// unavailable and the other path may well work; an aborted body, a stalled read or
// a content list above the chapter ceiling are all local conditions that the
// other path would fail on identically. Retrying those on the fallback route does
// not recover, and it buries the original failure — with --fallback=on-404 a
// deliberate --drop-after abort came back as a successful v2 download.
type statusError struct {
	status int
	path   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("toniebox: %s returned %d", e.path, e.status)
}

func (e *statusError) Unwrap() error { return nil }

// notFound reports whether err is a 404 from a content route.
func notFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

// FreshnessOutcome records what the box learned from a freshness check.
type FreshnessOutcome struct {
	Endpoint string
	Reported map[string]uint32
	Marked   []uint64
	Skipped  bool
}

// DownloadResult records one content request.
type DownloadResult struct {
	Path       string
	Range      string
	Status     int
	Advertised int64
	Received   int64
	FirstByte  time.Duration
	Duration   time.Duration
	AudioID    uint32
	HasTAF     bool
	Chapters   int
	// Dropped marks a body the box abandoned mid-stream by configuration, as
	// opposed to one the server ended. Only a drop can be resumed with a Range.
	Dropped bool
	Err     error
}

// PlacementResult records everything one placement produced.
type PlacementResult struct {
	UID           uint64
	Path          string
	Freshness     FreshnessOutcome
	Meta          *ContentMeta
	Downloads     []DownloadResult
	TotalReceived int64
	Duration      time.Duration
}

// DownloadProgress reports the milestones of a content request as they happen.
//
// The mock needs live timestamps rather than a result at the end, because the
// measurement the tool exists for is the gap between one placement's last byte
// and the next placement's first, and a placement is still in flight while the
// box is lifted.
type DownloadProgress struct {
	At         time.Time
	Path       string
	Range      string
	Status     int
	Advertised int64
	Bytes      int64
	AudioID    uint32
	HasTAF     bool
	Chapters   int
	Done       bool
	Err        error
}

// Downloader runs the box's side of a placement: freshness check, then content.
type Downloader struct {
	box    *Box
	client *Client
	rate   int64
	gate   *Gate

	// OnProgress, when set, receives every download milestone.
	OnProgress func(DownloadProgress)
}

// NewDownloader returns a downloader paced against rate bytes per second. A rate
// of zero means unlimited.
func NewDownloader(box *Box, client *Client, rate int64) *Downloader {
	return &Downloader{box: box, client: client, rate: rate, gate: NewGate()}
}

// Gate exposes the download's stall control.
func (d *Downloader) Gate() *Gate { return d.gate }

// FreshnessCheck runs the freshness exchange and returns what the box learned.
//
// For a stream-backed tonie the reply always marks the uid, regardless of the
// audio id reported: process_freshness_check marks every requested uid whose
// source type is CT_SOURCE_STREAM (src/handler_cloud.c:1031-1034). So a re-
// placement always re-requests the content, and the reported audio id does not
// change that. It is still worth sending faithfully, because it becomes
// decisive the moment the content source is not a stream.
func (d *Downloader) FreshnessCheck(ctx context.Context) (FreshnessOutcome, error) {
	cfg := d.box.Config()

	out := FreshnessOutcome{Reported: map[string]uint32{}}
	if cfg.Freshness == FreshnessSkip {
		out.Skipped = true
		return out, nil
	}
	if !cfg.Recheck {
		out.Skipped = true
		return out, nil
	}

	infos, err := d.box.FreshnessInfos()
	if err != nil {
		return out, fmt.Errorf("toniebox: building freshness report: %w", err)
	}
	if len(infos) == 0 {
		out.Skipped = true
		return out, nil
	}
	for _, info := range infos {
		out.Reported[RUIDHex(info.UID)] = info.AudioID
	}

	endpoint := "/v1/freshness-check"
	contentType := "application/octet-stream"
	var body []byte

	if cfg.Freshness == FreshnessV3 {
		endpoint = "/v3/freshness-check"
		contentType = "application/json"
		body, err = EncodeFreshnessRequestV3(infos)
		if err != nil {
			return out, err
		}
	} else {
		body, err = EncodeFreshnessRequest(infos)
		if err != nil {
			return out, err
		}
	}
	out.Endpoint = endpoint

	resp, err := d.client.Post(ctx, endpoint, contentType, body)
	if err != nil {
		return out, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("mock-toniebox: closing freshness response", "err", err)
		}
	}()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, MaxFreshnessBody))
	if err != nil {
		return out, fmt.Errorf("toniebox: reading freshness reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("toniebox: freshness check returned %d: %s", resp.StatusCode, payload)
	}

	if cfg.Freshness == FreshnessV3 {
		marked, err := DecodeFreshnessResponseV3(payload)
		if err != nil {
			return out, err
		}
		out.Marked = marked
		return out, nil
	}

	decoded, err := DecodeFreshnessResponse(payload)
	if err != nil {
		return out, err
	}
	out.Marked = decoded.Marked
	return out, nil
}

// Place runs a whole placement: freshness check, then content on whichever path
// the configuration selects, downloading at the configured rate.
func (d *Downloader) Place(ctx context.Context, uid uint64) (PlacementResult, error) {
	cfg := d.box.Config()
	start := time.Now()
	result := PlacementResult{UID: uid}

	freshness, err := d.FreshnessCheck(ctx)
	result.Freshness = freshness
	if err != nil {
		return result, err
	}

	offset := int64(0)
	if cfg.Range == RangeResume {
		offset = d.box.BytesReceived()
	}

	meta, download, err := d.fetch(ctx, uid, offset)
	result.Meta = meta
	result.Downloads = append(result.Downloads, download...)

	// A box that drops mid-body and comes back asks for Range: bytes=N-. Whether
	// teddyCloud resumes the existing TAF or starts a fresh ffmpeg is the
	// difference the shim has to cope with, and only this path produces it
	// (src/handler_cloud.c:645).
	if droppedFrom(download) && cfg.Range == RangeResume && d.box.BytesReceived() > 0 {
		slog.Info("mock-toniebox: re-requesting with a Range offset after the drop",
			"ruid", RUIDHex(uid), "offset", d.box.BytesReceived())
		resumedMeta, resumed, resumeErr := d.fetch(ctx, uid, d.box.BytesReceived())
		if resumedMeta != nil {
			result.Meta = resumedMeta
		}
		result.Downloads = append(result.Downloads, resumed...)
		if resumeErr != nil {
			err = resumeErr
		}
	}

	result.TotalReceived = d.box.BytesReceived()
	result.Path = pathOf(download)
	result.Duration = time.Since(start)
	return result, err
}

// droppedFrom reports whether any of these requests ended because the box walked
// away mid-body rather than because the route failed. The distinction is what
// makes the resume request meaningful: a drop leaves bytes on the ground, so the
// box comes back with a Range, whereas a 404 leaves nothing to resume.
func droppedFrom(results []DownloadResult) bool {
	for _, r := range results {
		if r.Dropped {
			return true
		}
	}
	return false
}

// pathOf reports which content route produced these results, so that a report
// names the route that was actually used. The configured path is not the same
// thing once a fallback has fired, and a report that claims "v3" for a placement
// served over v2 is worse than no report.
func pathOf(results []DownloadResult) string {
	for _, r := range results {
		switch {
		case strings.HasPrefix(r.Path, "/v3/chapter/"):
			return ContentV3.String()
		case strings.HasPrefix(r.Path, "/v2/content/"):
			return ContentV2.String()
		}
	}
	return ""
}

// fetch retrieves the content on the configured path, applying the fallback
// policy.
//
// Fallback keys on a 404 rather than on any error: a missing v3 route is the one
// condition the other path can plausibly survive, and treating every failure as
// fallback-eligible hid real ones — see statusError.
func (d *Downloader) fetch(ctx context.Context, uid uint64, offset int64) (*ContentMeta, []DownloadResult, error) {
	cfg := d.box.Config()

	// FallbackAlways means the box prefers v2 and treats v3 as its rescue path,
	// so it inverts which route is tried first. The configured path still wins
	// otherwise: the flag is about recovering from a missing route, not about
	// reordering a healthy one.
	preferV3 := cfg.ContentPath == ContentV3 && cfg.Fallback != FallbackAlways

	if preferV3 {
		meta, results, err := d.fetchV3(ctx, uid, offset)
		if err == nil {
			return meta, results, nil
		}
		if cfg.Fallback == FallbackNone || !notFound(err) {
			return meta, results, err
		}
		slog.Info("mock-toniebox: v3 content path is unavailable, falling back to v2",
			"err", err, "ruid", RUIDHex(uid))

		meta, v2results, v2err := d.fetchV2(ctx, uid, offset)
		if v2err != nil {
			return meta, append(results, v2results...), fmt.Errorf("toniebox: both content paths failed (v3: %v; v2: %w)", err, v2err)
		}
		return meta, append(results, v2results...), nil
	}

	meta, results, err := d.fetchV2(ctx, uid, offset)
	if err == nil {
		return meta, results, nil
	}
	if cfg.Fallback != FallbackAlways || !notFound(err) {
		return meta, results, err
	}

	slog.Info("mock-toniebox: v2 content path is unavailable, falling back to v3",
		"err", err, "ruid", RUIDHex(uid))

	meta, v3results, v3err := d.fetchV3(ctx, uid, offset)
	if v3err != nil {
		return meta, append(results, v3results...), fmt.Errorf("toniebox: both content paths failed (v2: %v; v3: %w)", err, v3err)
	}
	return meta, append(results, v3results...), nil
}

// fetchV3 walks the v3 route: content-meta for the chapter list and the audio
// id, then one request per chapter.
//
// An error here is returned rather than retried on v2 unless it is a 404, so a
// local fault stays a local fault. The route ordering that matters for the
// shim's Range question is the chapter requests, and a fallback in the middle of
// that list would change which route the shim sees bytes arrive on.
func (d *Downloader) fetchV3(ctx context.Context, uid uint64, offset int64) (*ContentMeta, []DownloadResult, error) {
	ruid := RUIDHex(uid)

	resp, err := d.client.Get(ctx, "/v3/content-meta/"+ruid, nil)
	if err != nil {
		return nil, nil, err
	}
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxFreshnessBody))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, nil, fmt.Errorf("toniebox: reading content-meta: %w", readErr)
	}
	if closeErr != nil {
		slog.Debug("mock-toniebox: closing content-meta response", "err", closeErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, &statusError{status: resp.StatusCode, path: "/v3/content-meta/" + ruid}
	}

	var meta ContentMeta
	if err := json.Unmarshal(payload, &meta); err != nil {
		return nil, nil, fmt.Errorf("toniebox: decoding content-meta: %w", err)
	}
	if len(meta.Content) > MaxChapters {
		return &meta, nil, fmt.Errorf("toniebox: content-meta lists %d chapters, teddyCloud caps at %d", len(meta.Content), MaxChapters)
	}

	// The audio id has to come from here rather than from the downloaded body:
	// /v3/chapter serves chapter 0 from offset 4096 with header_size=0, so no
	// TAF header ever appears in a v3 response
	// (src/handler_cloud.c:1535-1553) and ParseTAFHeader has nothing to read.
	// Without this the box never learns what it cached, reports 0 forever, and
	// the ContentAudioId correlation in a report can never fire.
	//
	// meta.Version is a uint64 in the JSON but a uint32 in the TAF header, so a
	// value that does not fit is ignored rather than truncated into an id that
	// silently never matches.
	var audioID uint32
	if meta.Version != 0 && meta.Version <= math.MaxUint32 {
		audioID = uint32(meta.Version)
		d.box.NoteDownloadedTAF(audioID)
	}

	results := make([]DownloadResult, 0, len(meta.Content))
	for i, item := range meta.Content {
		if ctx.Err() != nil {
			return &meta, results, ctx.Err()
		}
		chapterOffset := int64(0)
		if i > 0 {
			// Only the first chapter continues an interrupted download; the rest
			// start clean, and chapter 0 additionally skips the TAF header
			// (src/handler_cloud.c:1551-1553).
			chapterOffset = 0
		} else {
			chapterOffset = offset
		}
		result, err := d.download(ctx, "/v3/chapter/"+item.Name, chapterOffset)
		result = d.stampAudioID(result, audioID)
		if err != nil {
			if result.Dropped {
				// The box walked away mid-body, which is the scenario, not a
				// route failure. Hand the partial results back so Place can
				// resume from where the body stopped.
				return &meta, append(results, result), nil
			}
			return &meta, append(results, result), err
		}
		results = append(results, result)
	}

	return &meta, results, nil
}

// stampAudioID fills in the audio id a chapter body cannot carry, and republishes
// the milestone so the recorder sees it.
//
// Both parts are needed. A v3 body has no TAF header to parse the id from, so
// without the stamp a report's single-audio-id assertion passes vacuously on
// every v3 run — an assertion that cannot fail. And the recorder learns the id
// only from progress events, which download() emitted before the stamp existed,
// so without the re-publish the report still records zero.
//
// It is applied per result as the chapter completes rather than in a pass at the
// end, because the drop path returns early and would otherwise skip it.
func (d *Downloader) stampAudioID(result DownloadResult, audioID uint32) DownloadResult {
	if audioID == 0 || result.AudioID != 0 {
		return result
	}
	result.AudioID = audioID
	d.progress(result, DownloadProgress{
		At: time.Now(), Path: result.Path, Range: result.Range,
		Status: result.Status, Advertised: result.Advertised,
		Bytes: result.Received, AudioID: audioID, HasTAF: result.HasTAF,
		Chapters: result.Chapters, Done: true, Err: result.Err,
	})
	return result
}

func (d *Downloader) fetchV2(ctx context.Context, uid uint64, offset int64) (*ContentMeta, []DownloadResult, error) {
	result, err := d.download(ctx, "/v2/content/"+RUIDHex(uid), offset)
	results := []DownloadResult{result}
	if err != nil && !result.Dropped {
		return nil, results, err
	}
	return nil, results, nil
}

// download performs one content request and drains the body at the configured
// rate, capturing the TAF header from the front of it.
func (d *Downloader) download(ctx context.Context, path string, offset int64) (DownloadResult, error) {
	cfg := d.box.Config()

	result := DownloadResult{Path: path}
	if offset > 0 {
		result.Range = fmt.Sprintf("bytes=%d-", offset)
	}

	header := http.Header{}
	if offset > 0 {
		header.Set("Range", result.Range)
	}

	start := time.Now()
	resp, err := d.client.Get(ctx, path, header)
	if err != nil {
		result.Err = err
		result.Duration = time.Since(start)
		d.progress(result, DownloadProgress{At: time.Now(), Done: true, Err: err})
		return result, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("mock-toniebox: closing content response", "path", path, "err", err)
		}
	}()

	result.Status = resp.StatusCode
	result.Advertised = resp.ContentLength
	d.progress(result, DownloadProgress{
		At: time.Now(), Path: path, Range: result.Range,
		Status: resp.StatusCode, Advertised: resp.ContentLength,
	})

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		result.Err = &statusError{
			status: resp.StatusCode,
			path:   fmt.Sprintf("%s: %s", path, bytes.TrimSpace(body)),
		}
		result.Duration = time.Since(start)
		d.progress(result, DownloadProgress{At: time.Now(), Done: true, Err: result.Err})
		return result, result.Err
	}

	pacer := &pacer{rate: d.rate, start: time.Now(), stop: d.gate}
	if cfg.StallMS > 0 {
		d.gate.Block()
		time.AfterFunc(time.Duration(cfg.StallMS)*time.Millisecond, d.gate.Resume)
	}

	headerBuf := make([]byte, 0, TAFHeaderLength)
	var received int64

	for {
		if err := ctx.Err(); err != nil {
			result.Err = err
			break
		}
		if err := pacer.wait(ctx); err != nil {
			result.Err = err
			break
		}

		buf := make([]byte, 32*1024)
		n, err := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if received == 0 {
				result.FirstByte = time.Since(start)
			}
			received += int64(n)
			d.box.AddBytes(int64(n))
			if len(headerBuf) < TAFHeaderLength {
				need := TAFHeaderLength - len(headerBuf)
				if need > len(chunk) {
					need = len(chunk)
				}
				headerBuf = append(headerBuf, chunk[:need]...)
			}
			pacer.advance(int64(n))
			d.progress(result, DownloadProgress{At: time.Now(), Bytes: received})
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				result.Err = fmt.Errorf("reading %s: %w", path, err)
			}
			break
		}
		if cfg.MaxDownload > 0 && received >= cfg.MaxDownload {
			slog.Info("mock-toniebox: download paused at the configured maximum", "path", path, "bytes", received)
			break
		}
		if cfg.DropAfter > 0 && received >= cfg.DropAfter {
			result.Dropped = true
			result.Err = fmt.Errorf("mock-toniebox: dropping %s after %d bytes as configured", path, received)
			break
		}
	}

	result.Received = received
	result.Duration = time.Since(start)

	if len(headerBuf) > 0 {
		if taf, err := ParseTAFHeader(headerBuf); err == nil {
			result.AudioID = taf.AudioID
			result.HasTAF = true
			result.Chapters = len(taf.TrackPageNums)
			d.box.NoteDownloadedTAF(taf.AudioID)
		} else {
			// A chapter body after the first, or a v3 chapter with skip_taf_header
			// set, starts straight at the Ogg header. That is expected, not a fault.
			slog.Debug("mock-toniebox: response carried no TAF header", "path", path, "err", err)
		}
	}

	d.progress(result, DownloadProgress{
		At: time.Now(), Path: path, Range: result.Range, Status: result.Status,
		Advertised: result.Advertised, Bytes: received, AudioID: result.AudioID,
		HasTAF: result.HasTAF, Chapters: result.Chapters, Done: true, Err: result.Err,
	})

	return result, result.Err
}

func (d *Downloader) progress(result DownloadResult, p DownloadProgress) {
	if d.OnProgress == nil {
		return
	}
	if p.Path == "" {
		p.Path = result.Path
	}
	if p.Range == "" {
		p.Range = result.Range
	}
	if p.Status == 0 {
		p.Status = result.Status
	}
	if p.Advertised == 0 {
		p.Advertised = result.Advertised
	}
	d.OnProgress(p)
}

// pacer gates reads so the box consumes at a chosen multiple of real time
// instead of draining instantly, which is the one thing a real consumer never
// does.
type pacer struct {
	rate  int64
	start time.Time
	sent  int64
	stop  *Gate
}

func (p *pacer) advance(n int64) { p.sent += n }

func (p *pacer) wait(ctx context.Context) error {
	if p.stop != nil {
		if err := p.stop.Wait(ctx); err != nil {
			return err
		}
	}
	if p.rate <= 0 {
		return nil
	}

	want := time.Duration(p.sent) * time.Second / time.Duration(p.rate)
	if elapsed := time.Since(p.start); want > elapsed {
		timer := time.NewTimer(want - elapsed)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
