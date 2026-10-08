package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/toniebox"
)

// measurement is one reported number, optionally with an expectation. An
// assertion without a measured value attached is not usable when a run turns up
// a different failure than the one being looked for.
type measurement struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Pass  *bool  `json:"pass,omitempty"`
	Note  string `json:"note,omitempty"`
}

func pass(v bool) *bool { return &v }

// cycle is one placement and the lift that ended it.
type cycle struct {
	N    int    `json:"n"`
	UID  string `json:"ruid"`
	Path string `json:"content_path"`

	Range      string `json:"range_requested"`
	Status     int    `json:"status"`
	Advertised int64  `json:"advertised_bytes"`
	Bytes      int64  `json:"received_bytes"`
	AudioID    uint32 `json:"taf_audio_id"`
	Chapters   int    `json:"chapters"`
	Err        string `json:"error,omitempty"`

	PlacedAt    time.Time     `json:"placed_at"`
	TagValidAt  time.Time     `json:"tag_valid_at,omitempty"`
	StartedAt   time.Time     `json:"playback_started_at,omitempty"`
	FirstByteAt time.Time     `json:"first_byte_at,omitempty"`
	LastByteAt  time.Time     `json:"last_byte_at,omitempty"`
	StoppedAt   time.Time     `json:"playback_stopped_at,omitempty"`
	AudioIDAt   time.Time     `json:"content_audio_id_at,omitempty"`
	Downloaded  time.Duration `json:"download_duration"`
}

// recorder accumulates cycles and the SSE events observed alongside them, and
// then turns both into the measurement table the tool exists to produce.
type recorder struct {
	mu sync.Mutex

	assumptions [][2]string
	overlay     string
	started     time.Time

	events  []observedEvent
	current *cycle
	cycles  []cycle

	// audioIDs is the distinct set of audio ids seen, in first-seen order. It is
	// what the ContentAudioId invariant is computed over: one unchanged content
	// source must keep the same id across every cycle, so more than one entry
	// means a re-render leaked through.
	audioIDs []uint32
}

func newRecorder(cfg toniebox.Config, overlay string) *recorder {
	return &recorder{
		assumptions: cfg.Assumptions(),
		overlay:     overlay,
		started:     time.Now(),
	}
}

func (r *recorder) note(evt observedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, evt)
	if r.current == nil {
		return
	}

	c := r.current
	payload := evt.payload()
	switch evt.Name {
	case "TagValid":
		if c.TagValidAt.IsZero() {
			c.TagValidAt = evt.At
		}
	case "playback":
		switch payload {
		case "started":
			if c.StartedAt.IsZero() {
				c.StartedAt = evt.At
			}
		case "stopped":
			c.StoppedAt = evt.At
		}
	case "ContentAudioId":
		c.AudioIDAt = evt.At
	case "ContentTitle":
		// recorded only so that a report can show what the box displayed
	}
}

// progress folds a download milestone into the active cycle.
func (r *recorder) progress(p toniebox.DownloadProgress) {
	r.mu.Lock()
	defer r.mu.Unlock()

	c := r.current
	if c == nil {
		return
	}
	if p.Status != 0 {
		c.Status = p.Status
	}
	if p.Advertised != 0 {
		c.Advertised = p.Advertised
	}
	if p.Range != "" {
		c.Range = p.Range
	}
	if p.Path != "" {
		// The route the bytes actually arrived on. Recording only the configured
		// path would report a placement served by a v2 fallback as v3, which
		// sends whoever reads the report looking in the wrong place.
		switch {
		case strings.HasPrefix(p.Path, "/v3/chapter/"):
			c.Path = toniebox.ContentV3.String()
		case strings.HasPrefix(p.Path, "/v2/content/"):
			c.Path = toniebox.ContentV2.String()
		}
	}
	if p.Bytes > 0 {
		c.Bytes = p.Bytes
		c.LastByteAt = p.At
		if c.FirstByteAt.IsZero() {
			c.FirstByteAt = p.At
		}
	}
	if p.Done {
		if p.AudioID != 0 {
			c.AudioID = p.AudioID
		}
		if p.Chapters > 0 {
			c.Chapters = p.Chapters
		}
		if p.Err != nil {
			c.Err = p.Err.Error()
		}
	}
}

// begin starts the active cycle.
func (r *recorder) begin(n int, uid uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.current = &cycle{
		N:        n,
		UID:      toniebox.RUIDHex(uid),
		Path:     "",
		PlacedAt: time.Now(),
	}
}

// finish closes the active cycle, files it, and returns it.
func (r *recorder) finish() cycle {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.current == nil {
		return cycle{}
	}
	c := *r.current
	if !c.FirstByteAt.IsZero() {
		c.Downloaded = c.LastByteAt.Sub(c.FirstByteAt)
	}
	r.current = nil
	r.cycles = append(r.cycles, c)
	if c.AudioID != 0 {
		seen := false
		for _, id := range r.audioIDs {
			if id == c.AudioID {
				seen = true
				break
			}
		}
		if !seen {
			r.audioIDs = append(r.audioIDs, c.AudioID)
		}
	}
	return c
}

func (r *recorder) snapshot() (cycles []cycle, events []observedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()

	events = append(events, r.events...)
	cycles = append(cycles, r.cycles...)
	if r.current != nil {
		cycles = append(cycles, *r.current)
	}
	return cycles, events
}

// report renders the run: the assumptions that produced it, every measurement,
// and the raw cycles behind them. The assumption set is stamped first so that a
// report kept from an earlier run cannot be mistaken for a later one.
func (r *recorder) report(cycles []cycle) map[string]any {
	r.mu.Lock()
	events := append([]observedEvent(nil), r.events...)
	audioIDs := append([]uint32(nil), r.audioIDs...)
	assumptions := append([][2]string(nil), r.assumptions...)
	overlay := r.overlay
	elapsed := time.Since(r.started)
	r.mu.Unlock()

	return map[string]any{
		"overlay_id":   overlay,
		"assumptions":  assumptions,
		"elapsed":      elapsed.String(),
		"measurements": r.measurements(cycles, events, audioIDs),
		"cycles":       cycles,
		"events":       events,
	}
}

func (r *recorder) measurements(cycles []cycle, events []observedEvent, audioIDs []uint32) []measurement {
	out := []measurement{
		{Name: "cycles", Value: fmt.Sprint(len(cycles)), Pass: pass(len(cycles) > 0),
			Note: "placements completed"},
		{Name: "observed_events", Value: fmt.Sprint(len(events))},
	}

	var stops, starts int
	for _, evt := range events {
		switch evt.Name {
		case "playback":
			switch evt.payload() {
			case "started":
				starts++
			case "stopped":
				stops++
			}
		}
	}
	out = append(out,
		measurement{Name: "playback_started_events", Value: fmt.Sprint(starts)},
		measurement{Name: "playback_stopped_events", Value: fmt.Sprint(stops)},
		measurement{Name: "stopped_per_cycle",
			Value: fmt.Sprintf("%.2f", ratio(stops, len(cycles))),
			Pass:  pass(len(cycles) == 0 || stops == len(cycles)),
			Note:  "a lift must produce exactly one playback=stopped and nothing else must",
		},
	)

	// The number the whole tool exists for: how long the box sat with nothing to
	// read between one placement and the next.
	var gaps []time.Duration
	for i := 1; i < len(cycles); i++ {
		if cycles[i].FirstByteAt.IsZero() || cycles[i-1].LastByteAt.IsZero() {
			continue
		}
		gaps = append(gaps, cycles[i].FirstByteAt.Sub(cycles[i-1].LastByteAt))
	}
	if len(gaps) > 0 {
		var maxGap time.Duration
		var total time.Duration
		for _, g := range gaps {
			total += g
			if g > maxGap {
				maxGap = g
			}
		}
		out = append(out, measurement{
			Name:  "inter_cycle_gap_mean_ms",
			Value: fmt.Sprint(total.Milliseconds() / int64(len(gaps))),
			Note:  "last byte of cycle N to first byte of cycle N+1 — the silence window",
		}, measurement{
			Name:  "inter_cycle_gap_max_ms",
			Value: fmt.Sprint(maxGap.Milliseconds()),
			Note:  "worst observed silence between placements",
		})
	}

	out = append(out, measurement{
		Name:  "content_audio_ids",
		Value: fmt.Sprintf("%v", audioIDs),
		Note:  "must be a single id for one unchanged content source",
		Pass:  pass(len(audioIDs) <= 1),
	})

	if len(cycles) > 0 {
		var bytes int64
		var nonzero int
		for _, c := range cycles {
			bytes += c.Bytes
			if c.Bytes > 0 {
				nonzero++
			}
		}
		out = append(out,
			measurement{Name: "bytes_received_total", Value: fmt.Sprint(bytes)},
			measurement{Name: "cycles_with_content",
				Value: fmt.Sprint(nonzero),
				Pass:  pass(nonzero == len(cycles)),
				Note:  "every placement must produce content bytes",
			},
		)

		var failures int
		for _, c := range cycles {
			if c.Err != "" {
				failures++
			}
		}
		out = append(out, measurement{
			Name:  "failed_cycles",
			Value: fmt.Sprint(failures),
			Pass:  pass(failures == 0),
			Note:  "report cycles[].error for the failing requests",
		})

		uids := map[string]bool{}
		for _, c := range cycles {
			uids[c.UID] = true
		}
		out = append(out, measurement{
			Name:  "distinct_ruids",
			Value: fmt.Sprint(len(uids)),
			Pass:  pass(len(uids) == 1),
			Note:  "a single-figurine run must report one rUID on every cycle",
		})
	}

	return out
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// writeReport serialises a report with stable formatting.
func writeReport(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	return append(b, '\n')
}
