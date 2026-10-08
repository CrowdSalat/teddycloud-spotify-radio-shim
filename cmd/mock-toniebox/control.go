package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/toniebox"
)

// action is one of the verbs the control surface accepts. Every one maps to a
// frame a real box emits; nothing here synthesises an event teddyCloud could not
// have received from hardware.
type action string

// Control actions.
const (
	actPlace         action = "place"
	actLift          action = "lift"
	actInvalid       action = "invalid"
	actPlayback      action = "playback"
	actKnock         action = "knock"
	actTilt          action = "tilt"
	actCharger       action = "charger"
	actEar           action = "ear"
	actAudioID       action = "audio-id"
	actVolume        action = "volume"
	actCycle         action = "cycle"
	actStall         action = "stall"
	actResume        action = "resume"
	actDisconnect    action = "disconnect"
	actTelemetry     action = "telemetry"
	actReportAudioID action = "report-audio-id"
)

func (m *mock) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/control/")
	verb, rest, _ := strings.Cut(path, "/")
	q := r.URL.Query()

	switch action(verb) {
	case actPlace:
		uid, err := m.uidFromQuery(q)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		res, err := m.place(r.Context(), uid, *holdFlag)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, res)

	case actLift:
		uid, err := m.uidFromQuery(q)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := m.lift(uid); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case actInvalid:
		uid, err := m.uidFromQuery(q)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := m.emitTag(toniebox.TagInvalid, uid); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case actPlayback:
		if err := m.emitLog3(rest, log3Playback); err != nil {
			reject(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case actKnock:
		if err := m.emitLog3(rest, log3Knock); err != nil {
			reject(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case actTilt:
		if err := m.emitLog3(rest, log3Tilt); err != nil {
			reject(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case actCharger:
		if err := m.emitLog3(rest, log3Charger); err != nil {
			reject(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case actEar:
		double := rest == "double" || q.Get("double") == "true"
		ear := strings.TrimSuffix(rest, "/double")
		if err := m.emitEar(ear, double, q); err != nil {
			reject(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case actVolume:
		level, _ := strconv.Atoi(q.Get("level"))
		db, _ := strconv.Atoi(q.Get("db"))
		frames, err := m.box.VolumeFrames(int32(level), int32(db))
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if err := m.send(frames...); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case actAudioID:
		id, err := strconv.ParseUint(q.Get("id"), 10, 32)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("id must be an unsigned 32-bit number"))
			return
		}
		frames, err := m.box.AudioIDFrames(uint32(id))
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if err := m.send(frames...); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case actReportAudioID:
		id, ok := m.box.LastTAFAudioID()
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("no TAF has been downloaded yet"))
			return
		}
		frames, err := m.box.AudioIDFrames(id)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if err := m.send(frames...); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]uint32{"audio_id": id})

	case actCycle:
		count := parseCount(r)
		gap := *gapFlag
		hold := *holdFlag
		if v := q.Get("gap"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				gap = d
			}
		}
		if v := q.Get("hold"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				hold = d
			}
		}
		if err := m.runCycle(r.Context(), count, gap, hold); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		cycles, _ := m.rec.snapshot()
		writeJSON(w, http.StatusOK, m.rec.report(cycles))

	case actStall:
		m.downloader.Gate().Block()
		ms, _ := strconv.Atoi(q.Get("ms"))
		if ms > 0 {
			time.AfterFunc(time.Duration(ms)*time.Millisecond, m.downloader.Gate().Resume)
		}
		writeJSON(w, http.StatusOK, map[string]any{"stalled": true, "resume_ms": ms})

	case actResume:
		m.downloader.Gate().Resume()
		writeJSON(w, http.StatusOK, map[string]any{"stalled": false})

	case actDisconnect:
		m.client.DisconnectRTNL()
		writeJSON(w, http.StatusOK, map[string]any{"disconnected": true})

	case actTelemetry:
		frames, err := m.box.TelemetryFrames()
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if err := m.send(frames...); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"frames": len(frames)})

	default:
		writeErr(w, http.StatusNotFound, fmt.Errorf("unknown action %q", verb))
	}
}

func (m *mock) uidFromQuery(q map[string][]string) (uint64, error) {
	values, ok := q["uid"]
	if !ok || len(values) == 0 || values[0] == "" {
		return m.box.Config().UID, nil
	}
	return toniebox.ParseRUID(values[0])
}

func (m *mock) emitTag(kind toniebox.TagKind, uid uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	frames, err := m.box.TagFrames(kind, uid)
	if err != nil {
		return err
	}
	return m.send(frames...)
}

// badRequest marks an error the caller caused, as opposed to one in reaching
// teddyCloud. Keeping the two apart is what lets an unknown verb answer 400
// rather than 502: reporting a typo as an upstream failure sends the operator
// looking at teddyCloud instead of at their own command.
type badRequest struct{ err error }

func (e *badRequest) Error() string { return e.err.Error() }
func (e *badRequest) Unwrap() error { return e.err }

func reject(w http.ResponseWriter, err error) {
	var br *badRequest
	if errors.As(err, &br) {
		writeErr(w, http.StatusBadRequest, br)
		return
	}
	writeErr(w, http.StatusBadGateway, err)
}

// log3 groups the event names each action verb accepts, so that an unknown verb
// reports which values were legal rather than failing silently.
type log3Group struct {
	names  map[string]uint32
	plural string
}

var (
	log3Playback = log3Group{names: map[string]uint32{
		"starting": toniebox.Log3PlaybackStarting,
		"started":  toniebox.Log3PlaybackStarted,
		"stopped":  toniebox.Log3PlaybackStopped,
	}, plural: "starting, started, stopped"}
	log3Knock = log3Group{names: map[string]uint32{
		"forward":  toniebox.Log3KnockForward,
		"backward": toniebox.Log3KnockBackward,
	}, plural: "forward, backward"}
	log3Tilt = log3Group{names: map[string]uint32{
		"forward":  toniebox.Log3TiltForward,
		"backward": toniebox.Log3TiltBackward,
	}, plural: "forward, backward"}
	log3Charger = log3Group{names: map[string]uint32{
		"on":  toniebox.Log3ChargerOn,
		"off": toniebox.Log3ChargerOff,
	}, plural: "on, off"}
)

func (m *mock) emitLog3(value string, group log3Group) error {
	eventType, ok := group.names[value]
	if !ok {
		return &badRequest{fmt.Errorf("%s must be one of %s", value, group.plural)}
	}
	frames, err := m.box.Log3Frames(eventType)
	if err != nil {
		return err
	}
	return m.send(frames...)
}

// emitEar sends a volume frame followed by the press as two separate frames.
// They cannot share one RPC: teddyCloud evaluates log3 before log2 inside a
// single frame, so a combined frame would arm the double-press window only after
// the press had already been judged (src/handler_rtnl.c:262` then `:332`).
func (m *mock) emitEar(ear string, double bool, q map[string][]string) error {
	eventType, ok := map[string]uint32{
		"big":   toniebox.Log3EarBig,
		"small": toniebox.Log3EarSmall,
	}[ear]
	if !ok {
		return &badRequest{fmt.Errorf("ear must be one of %s", strings.Join(earNames, ", "))}
	}

	level, db := 12, -3
	if values, found := q["level"]; found && len(values) > 0 {
		_, _ = fmt.Sscanf(values[0], "%d", &level)
	}
	if values, found := q["db"]; found && len(values) > 0 {
		_, _ = fmt.Sscanf(values[0], "%d", &db)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	frames, err := m.box.EarPressFrames(eventType, double, int32(level), int32(db))
	if err != nil {
		return err
	}
	return m.send(frames...)
}

// earNames is the set /control/ear accepts, kept next to emitEar so the two cannot
// drift apart.
var earNames = []string{"big", "small"}

// handleReport serves the accumulated measurement table. It is separate from
// /control/report in the mux because it is a read, not an action.
func (m *mock) handleReport(w http.ResponseWriter, r *http.Request) {
	cycles, _ := m.rec.snapshot()
	writeJSON(w, http.StatusOK, m.rec.report(cycles))
}

// handleSSE mirrors the events the observer has seen, so a run can be watched in
// a terminal without also running a tail on teddyCloud.
func (m *mock) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	keepAlive := time.NewTicker(16 * time.Second)
	defer keepAlive.Stop()

	var last int

	for {
		_, current := m.rec.snapshot()
		for ; last < len(current); last++ {
			payload, err := json.Marshal(current[last])
			if err != nil {
				slog.Debug("mock-toniebox: encoding mirrored event", "err", err)
				continue
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", current[last].Name, payload)
			flusher.Flush()
		}

		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// handleIndex serves the control page.
func (m *mock) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, indexHTML)
}

const indexHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Mock Toniebox</title>
<style>
body { font-family: sans-serif; margin: 2rem; max-width: 52rem; }
button { margin: .2rem .2rem 0 0; padding: .5rem .9rem; font-size: 1rem; }
fieldset { margin-top: 1rem; border: 1px solid #ccc; }
legend { font-weight: bold; }
pre { background: #f4f4f4; padding: .8rem; overflow-x: auto; }
#events { height: 14rem; overflow-y: auto; }
</style>
</head>
<body>
<h1>Mock Toniebox control</h1>
<p>Drives a real teddyCloud as a box would. Every action below is an RTNL frame a
physical box emits; teddyCloud performs the translation into SSE.</p>

<fieldset><legend>Figurine</legend>
<button onclick="post('/control/place')">Place</button>
<button onclick="post('/control/lift')">Lift</button>
<button onclick="post('/control/invalid')">Tag invalid</button>
<button onclick="post('/control/cycle?count=5')">5 cycles</button>
<button onclick="post('/control/cycle?count=50')">50 cycles</button>
</fieldset>

<fieldset><legend>Playback</legend>
<button onclick="post('/control/playback/starting')">Starting</button>
<button onclick="post('/control/playback/started')">Started</button>
<button onclick="post('/control/playback/stopped')">Stopped</button>
<button onclick="post('/control/report-audio-id')">Audio id</button>
</fieldset>

<fieldset><legend>Gestures</legend>
<button onclick="post('/control/knock/forward')">Knock &rarr;</button>
<button onclick="post('/control/knock/backward')">Knock &larr;</button>
<button onclick="post('/control/tilt/forward')">Tilt &rarr;</button>
<button onclick="post('/control/tilt/backward')">Tilt &larr;</button>
<button onclick="post('/control/charger/on')">Charger on</button>
<button onclick="post('/control/charger/off')">Charger off</button>
</fieldset>

<fieldset><legend>Box volume (never changes the track)</legend>
<button onclick="post('/control/ear/big')">Ear pinch up</button>
<button onclick="post('/control/ear/big/double')">Ear pinch up double</button>
<button onclick="post('/control/ear/small')">Ear pinch down</button>
<button onclick="post('/control/volume?level=12&amp;db=-3')">Report level</button>
</fieldset>

<fieldset><legend>Faults</legend>
<button onclick="post('/control/stall?ms=3000')">Stall 3s</button>
<button onclick="post('/control/resume')">Resume</button>
<button onclick="post('/control/disconnect')">Drop RTNL</button>
<button onclick="post('/control/telemetry')">Send telemetry</button>
</fieldset>

<fieldset><legend>Observation</legend>
<button onclick="load('/control/report', true)">Load report</button>
<pre id="out">report appears here</pre>
<h3>Observed SSE</h3>
<pre id="events"></pre>
</fieldset>

<script>
function post(url) {
  fetch(url, {method: 'POST'}).then(function (r) {
    return r.text().then(function (t) {
      var out = document.getElementById('out');
      out.textContent = url + ' -> ' + r.status + '\n\n' + t;
    });
  });
}
function load(url, pretty) {
  fetch(url).then(function (r) { return r.text(); }).then(function (t) {
    document.getElementById('out').textContent = t;
  });
}
(function () {
  var log = document.getElementById('events');
  var es = new EventSource('/api/sse');
  es.onmessage = function (e) { };
  es.addEventListener('event', function (e) { });
  ['TagValid', 'TagInvalid', 'playback', 'ContentAudioId', 'ContentTitle',
   'knock', 'tilt', 'pressed', 'charger', 'keep-alive'].forEach(function (name) {
    es.addEventListener(name, function (e) {
      log.textContent += name + ' ' + e.data + '\n';
      log.scrollTop = log.scrollHeight;
    });
  });
  // teddyCloud names every event in an event: field, so also listen generically
  es.onmessage = function (e) {
    log.textContent += 'message ' + e.data + '\n';
    log.scrollTop = log.scrollHeight;
  };
})();
</script>
</body>
</html>
`
