package ios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DIYgod/pinky/internal/driver"
	"github.com/DIYgod/pinky/internal/screen"
)

// liveWDA is a WebDriverAgent that records the actions it is sent: when
// they arrive, what they say, and whether two of them ran at the same time.
type liveWDA struct {
	mu       sync.Mutex
	actions  []liveAction
	fail     int           // how many of the first actions fail
	running  int           // the actions in flight
	overlaps bool          // two actions ran at the same time
	delay    time.Duration // how long an action takes
}

// liveAction is one gesture the phone was sent.
type liveAction struct {
	at   time.Time
	body string
}

func (f *liveWDA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if i := strings.Index(path, "/session/"); i == 0 {
		rest := strings.TrimPrefix(path, "/session/")
		if j := strings.IndexByte(rest, '/'); j > 0 {
			path = rest[j:]
		}
	}
	switch {
	case r.Method == http.MethodPost && path == "/actions":
		f.action(w, r)
	case r.Method == http.MethodPost && path == "/session":
		fmt.Fprint(w, `{"value":{"sessionId":"s1","capabilities":{}},"sessionId":"s1"}`)
	case r.Method == http.MethodPost && path == "/appium/settings":
		fmt.Fprint(w, `{"value":null}`)
	default:
		w.WriteHeader(404)
		fmt.Fprint(w, `{"value":null}`)
	}
}

// action records one gesture and answers it.
func (f *liveWDA) action(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.running++
	if f.running > 1 {
		f.overlaps = true
	}
	f.actions = append(f.actions, liveAction{at: time.Now(), body: string(b)})
	n := len(f.actions)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if n <= f.fail {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"value":{"error":"unknown error","message":"the gesture failed"}}`)
		return
	}
	fmt.Fprint(w, `{"value":null}`)
}

// got returns the actions the phone was sent.
func (f *liveWDA) got() []liveAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]liveAction(nil), f.actions...)
}

// count returns how many actions the phone was sent.
func (f *liveWDA) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.actions)
}

// livePath returns the points a gesture moves through, in screen pixels.
func livePath(t *testing.T, body string) []screen.Point {
	t.Helper()
	var out []screen.Point
	for _, a := range liveActs(t, body) {
		if a["type"] != "pointerMove" {
			continue
		}
		x, _ := a["x"].(float64)
		y, _ := a["y"].(float64)
		out = append(out, screen.Point{X: int(math.Round(x*3 - 0.5)), Y: int(math.Round(y*3 - 0.5))})
	}
	return out
}

// liveActs returns the pointer actions of one gesture.
func liveActs(t *testing.T, body string) []map[string]any {
	t.Helper()
	var p pointerActions
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 1 {
		t.Fatalf("%d pointers", len(p.Actions))
	}
	return p.Actions[0].Actions
}

// liveDur is how long a gesture takes on the phone.
func liveDur(t *testing.T, body string) time.Duration {
	t.Helper()
	var total time.Duration
	for _, a := range liveActs(t, body) {
		if a["type"] != "pointerMove" {
			continue
		}
		ms, _ := a["duration"].(float64)
		total += time.Duration(ms) * time.Millisecond
	}
	return total
}

// waitDrag waits until the player has played everything the finger
// recorded.
func waitDrag(t *testing.T, d *WDADevice) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if d.rec.dragDone() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the drag did not finish")
}

// liveDragOnce plays a drag on the phone: a point every 8 ms, and then the
// lift. It says where the finger lifted.
func liveDragOnce(t *testing.T, d *WDADevice, start screen.Point, moves int) screen.Point {
	t.Helper()
	ctx := context.Background()
	if err := d.Touch(ctx, driver.TouchDown, start); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= moves; i++ {
		p := screen.Point{X: start.X + i*10, Y: start.Y}
		if err := d.Touch(ctx, driver.TouchMove, p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(8 * time.Millisecond)
	}
	up := screen.Point{X: start.X + (moves+1)*10, Y: start.Y}
	if err := d.Touch(ctx, driver.TouchUp, up); err != nil {
		t.Fatal(err)
	}
	waitDrag(t, d)
	return up
}

// safeBuf is a buffer the player may write to while the test reads it.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// logValue reads one value out of a line of the log.
func logValue(t *testing.T, line, key string) string {
	t.Helper()
	i := strings.Index(line, key+"=")
	if i < 0 {
		t.Fatalf("the line does not have %s: %s", key, line)
	}
	rest := line[i+len(key)+1:]
	if j := strings.IndexAny(rest, " \n"); j >= 0 {
		rest = rest[:j]
	}
	return strings.Trim(rest, `"`)
}

// TestLiveDragTap checks that a tap is played as a tap, when the finger
// lifts, and that nothing goes to the phone before that.
func TestLiveDragTap(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	ctx := context.Background()
	p := screen.Point{X: 300, Y: 1200}
	if err := d.Touch(ctx, driver.TouchDown, p); err != nil {
		t.Fatal(err)
	}
	if n := f.count(); n != 0 {
		t.Fatalf("%d actions after the press, want none", n)
	}
	if err := d.Touch(ctx, driver.TouchUp, p); err != nil {
		t.Fatal(err)
	}
	acts := f.got()
	if len(acts) != 1 {
		t.Fatalf("%d actions for a tap, want one", len(acts))
	}
	// The tap is a press and a pause at one point, as it was before.
	steps := liveActs(t, acts[0].body)
	if len(steps) != 4 || steps[0]["type"] != "pointerMove" || steps[1]["type"] != "pointerDown" ||
		steps[2]["type"] != "pause" || steps[3]["type"] != "pointerUp" {
		t.Fatalf("a tap is %v", steps)
	}
	if ms, _ := steps[2]["duration"].(float64); ms != 60 {
		t.Errorf("the tap is held for %v ms, want 60", ms)
	}
	if path := livePath(t, acts[0].body); len(path) != 1 || path[0] != p {
		t.Errorf("the tap is at %v, want %v", path, p)
	}
}

// TestLiveDragLongPress checks that a press that is held and lifted without
// moving is a long press, as it was before.
func TestLiveDragLongPress(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	ctx := context.Background()
	p := screen.Point{X: 300, Y: 1200}
	if err := d.Touch(ctx, driver.TouchDown, p); err != nil {
		t.Fatal(err)
	}
	time.Sleep(800 * time.Millisecond)
	if n := f.count(); n != 0 {
		t.Fatalf("%d actions while the finger rested, want none", n)
	}
	if err := d.Touch(ctx, driver.TouchUp, p); err != nil {
		t.Fatal(err)
	}
	acts := f.got()
	if len(acts) != 1 {
		t.Fatalf("%d actions for a long press, want one", len(acts))
	}
	steps := liveActs(t, acts[0].body)
	if len(steps) != 4 || steps[2]["type"] != "pause" {
		t.Fatalf("a long press is %v", steps)
	}
	if ms, _ := steps[2]["duration"].(float64); ms != 800 {
		t.Errorf("the press is held for %v ms, want 800", ms)
	}
}

// TestLiveDragFollows checks that a drag on the mirror moves the phone
// while the finger is down: the phone is sent several gestures, the first
// one before the finger lifts, and they follow each other without a jump.
func TestLiveDragFollows(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	ctx := context.Background()
	start := screen.Point{X: 100, Y: 1000}
	if err := d.Touch(ctx, driver.TouchDown, start); err != nil {
		t.Fatal(err)
	}
	// The drag: a point every 8 ms for 400 ms.
	const moves = 50
	for i := 1; i <= moves; i++ {
		p := screen.Point{X: start.X + i*10, Y: start.Y}
		if err := d.Touch(ctx, driver.TouchMove, p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(8 * time.Millisecond)
	}
	// The phone moves while the finger is down: a gesture arrived before
	// the lift. This is what the whole change is about.
	up := screen.Point{X: start.X + (moves+1)*10, Y: start.Y}
	upAt := time.Now()
	if err := d.Touch(ctx, driver.TouchUp, up); err != nil {
		t.Fatal(err)
	}
	waitDrag(t, d)
	acts := f.got()
	if len(acts) <= 1 {
		t.Fatalf("%d actions for a drag, want more than one", len(acts))
	}
	early := 0
	for _, a := range acts {
		if a.at.Before(upAt) {
			early++
		}
	}
	t.Logf("%d actions on the phone, %d of them before the finger lifted", len(acts), early)
	if early == 0 {
		t.Errorf("no action arrived before the finger lifted (%d actions)", len(acts))
	}
	if f.overlaps {
		t.Error("two actions ran at the same time")
	}
	// The gestures follow each other: the first point of a gesture is
	// where the last one ended, and the last gesture ends where the
	// finger lifted.
	var prev screen.Point
	for i, a := range acts {
		path := livePath(t, a.body)
		if len(path) < 2 {
			t.Fatalf("action %d moves through %v", i, path)
		}
		if i > 0 && path[0] != prev {
			t.Errorf("action %d starts at %v, want %v", i, path[0], prev)
		}
		prev = path[len(path)-1]
		if d := liveDur(t, a.body); d > 2*liveSegMax {
			t.Errorf("action %d takes %v on the phone, want no more than %v", i, d, 2*liveSegMax)
		}
	}
	if prev != up {
		t.Errorf("the drag ends at %v, want %v", prev, up)
	}
}

// TestLiveDragLongPath checks that a long drag is still played to the end
// after the recorder has kept its path short: the pieces go on from where
// the phone's finger is, never back.
func TestLiveDragLongPath(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	ctx := context.Background()
	start := screen.Point{X: 100, Y: 1000}
	if err := d.Touch(ctx, driver.TouchDown, start); err != nil {
		t.Fatal(err)
	}
	// More points than the recorder keeps, so it drops some on the way.
	const moves = recMaxPoints + 10
	for i := 1; i <= moves; i++ {
		p := screen.Point{X: start.X + i*5, Y: start.Y}
		if err := d.Touch(ctx, driver.TouchMove, p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(17 * time.Millisecond)
	}
	up := screen.Point{X: start.X + (moves+1)*5, Y: start.Y}
	if err := d.Touch(ctx, driver.TouchUp, up); err != nil {
		t.Fatal(err)
	}
	waitDrag(t, d)
	acts := f.got()
	if len(acts) <= 1 {
		t.Fatalf("%d actions for a long drag, want more than one", len(acts))
	}
	if f.overlaps {
		t.Error("two actions ran at the same time")
	}
	var prev screen.Point
	for i, a := range acts {
		path := livePath(t, a.body)
		if len(path) < 2 {
			t.Fatalf("action %d moves through %v", i, path)
		}
		// A point dropped from the path may take the piece a little
		// forward, never back.
		if i > 0 && (path[0].X < prev.X || path[0].X-prev.X > 2*recMinMove) {
			t.Errorf("action %d starts at %v, the one before ended at %v", i, path[0], prev)
		}
		prev = path[len(path)-1]
	}
	if prev != up {
		t.Errorf("the drag ends at %v, want %v", prev, up)
	}
}

// TestLiveDragFails checks that a gesture that failed is told on the next
// touch, once, and that the touch after it works again.
func TestLiveDragFails(t *testing.T) {
	f := &liveWDA{fail: 1} // the first gesture fails
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	ctx := context.Background()
	start := screen.Point{X: 100, Y: 1000}
	var told int
	fail := func(err error) {
		if err != nil {
			told++
		}
	}
	fail(d.Touch(ctx, driver.TouchDown, start))
	for i := 1; i <= 50; i++ {
		p := screen.Point{X: start.X + i*10, Y: start.Y}
		fail(d.Touch(ctx, driver.TouchMove, p))
		time.Sleep(8 * time.Millisecond)
	}
	fail(d.Touch(ctx, driver.TouchUp, screen.Point{X: start.X + 510, Y: start.Y}))
	waitDrag(t, d)
	if told == 0 {
		t.Fatal("the failure was not told")
	}
	// It is told once: the next touch works again.
	if err := d.Touch(ctx, driver.TouchMove, screen.Point{X: start.X + 520, Y: start.Y}); err != nil {
		t.Errorf("the touch after the failure: %v", err)
	}
	// And a tap is a tap again.
	p := screen.Point{X: 300, Y: 1200}
	if err := d.Touch(ctx, driver.TouchDown, p); err != nil {
		t.Fatal(err)
	}
	if err := d.Touch(ctx, driver.TouchUp, p); err != nil {
		t.Fatal(err)
	}
	acts := f.got()
	last := acts[len(acts)-1]
	steps := liveActs(t, last.body)
	if len(steps) != 4 || steps[2]["type"] != "pause" {
		t.Fatalf("the tap after the failure is %v", steps)
	}
}

// TestLiveDragHolds checks that a drag is played in pieces that lift at a
// standstill: every piece but the last rests before the finger lifts, so
// the phone does not read the piece as a flick and fling the content away.
// The last piece is the person's own lift and keeps its speed.
func TestLiveDragHolds(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	start := screen.Point{X: 100, Y: 1000}
	up := liveDragOnce(t, d, start, 50)
	acts := f.got()
	if len(acts) < 2 {
		t.Fatalf("%d pieces for a drag, want more than one", len(acts))
	}
	for i, a := range acts {
		steps := liveActs(t, a.body)
		if steps[len(steps)-1]["type"] != "pointerUp" {
			t.Fatalf("piece %d ends with %v, want pointerUp", i, steps[len(steps)-1]["type"])
		}
		if i == len(acts)-1 {
			// The last piece is the person's own lift: it keeps its speed,
			// so the finger does not rest before it.
			for _, s := range steps {
				if s["type"] == "pause" {
					t.Fatalf("the last piece rests: %v", steps)
				}
			}
			if steps[len(steps)-2]["type"] != "pointerMove" {
				t.Errorf("the last piece is %v, want a move before the lift", steps)
			}
			continue
		}
		// Every other piece rests before it lifts.
		pause := steps[len(steps)-2]
		if pause["type"] != "pause" {
			t.Fatalf("piece %d lifts without a rest: %v", i, steps)
		}
		if ms, _ := pause["duration"].(float64); ms != float64(liveHold.Milliseconds()) {
			t.Errorf("piece %d rests for %v ms, want %v", i, ms, liveHold.Milliseconds())
		}
	}
	// The pieces follow each other and end where the finger lifted.
	var prev screen.Point
	for i, a := range acts {
		path := livePath(t, a.body)
		if len(path) < 2 {
			t.Fatalf("piece %d moves through %v", i, path)
		}
		if i > 0 && path[0] != prev {
			t.Errorf("piece %d starts at %v, want %v", i, path[0], prev)
		}
		prev = path[len(path)-1]
	}
	if prev != up {
		t.Errorf("the drag ends at %v, want %v", prev, up)
	}
}

// TestLiveDragOnePiece checks that a drag that is one piece is played as
// the person lifted it: the piece is the last one, so it does not rest.
func TestLiveDragOnePiece(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	ctx := context.Background()
	start := screen.Point{X: 100, Y: 1000}
	if err := d.Touch(ctx, driver.TouchDown, start); err != nil {
		t.Fatal(err)
	}
	// The finger goes past the slop. The move is recorded without the
	// player, so the whole drag is one piece when the finger lifts.
	time.Sleep(recMinGap + 5*time.Millisecond)
	p := screen.Point{X: start.X + recTapSlop + 2, Y: start.Y}
	if _, err := d.rec.touch(driver.TouchMove, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	up := screen.Point{X: start.X + recTapSlop + 4, Y: start.Y}
	if err := d.Touch(ctx, driver.TouchUp, up); err != nil {
		t.Fatal(err)
	}
	waitDrag(t, d)
	acts := f.got()
	if len(acts) != 1 {
		t.Fatalf("%d pieces for a drag of one piece, want one", len(acts))
	}
	steps := liveActs(t, acts[0].body)
	for _, s := range steps {
		if s["type"] == "pause" {
			t.Fatalf("the only piece rests: %v", steps)
		}
	}
	if path := livePath(t, acts[0].body); path[len(path)-1] != up {
		t.Errorf("the drag ends at %v, want %v", path[len(path)-1], up)
	}
}

// TestLiveDragNoHold checks that with no rest the drag is played as it was
// before the rest was added: no piece pauses, and the drag still ends where
// the finger lifted.
func TestLiveDragNoHold(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	d.live.tune(liveSegMax, 0) // the finger never rests
	start := screen.Point{X: 100, Y: 1000}
	up := liveDragOnce(t, d, start, 50)
	acts := f.got()
	if len(acts) < 2 {
		t.Fatalf("%d pieces for a drag, want more than one", len(acts))
	}
	for i, a := range acts {
		steps := liveActs(t, a.body)
		for _, s := range steps {
			if s["type"] == "pause" {
				t.Fatalf("piece %d rests: %v", i, steps)
			}
		}
	}
	// The pieces still follow each other and end where the finger lifted.
	var prev screen.Point
	for i, a := range acts {
		path := livePath(t, a.body)
		if len(path) < 2 {
			t.Fatalf("piece %d moves through %v", i, path)
		}
		if i > 0 && path[0] != prev {
			t.Errorf("piece %d starts at %v, want %v", i, path[0], prev)
		}
		prev = path[len(path)-1]
	}
	if prev != up {
		t.Errorf("the drag ends at %v, want %v", prev, up)
	}
}

// TestLiveDragWhole checks that a drag that never went past the slop while
// the finger was down is played in one go, as it was before, and without a
// rest.
func TestLiveDragWhole(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	ctx := context.Background()
	start := screen.Point{X: 100, Y: 1000}
	if err := d.Touch(ctx, driver.TouchDown, start); err != nil {
		t.Fatal(err)
	}
	time.Sleep(recMinGap + 5*time.Millisecond)
	if err := d.Touch(ctx, driver.TouchMove, screen.Point{X: start.X + 10, Y: start.Y}); err != nil {
		t.Fatal(err)
	}
	// The finger lifts past the slop, but the drag never started: it is
	// played in one go.
	up := screen.Point{X: start.X + 40, Y: start.Y}
	if err := d.Touch(ctx, driver.TouchUp, up); err != nil {
		t.Fatal(err)
	}
	acts := f.got()
	if len(acts) != 1 {
		t.Fatalf("%d pieces for a whole drag, want one", len(acts))
	}
	for _, s := range liveActs(t, acts[0].body) {
		if s["type"] == "pause" {
			t.Fatalf("the whole drag rests: %v", liveActs(t, acts[0].body))
		}
	}
	if path := livePath(t, acts[0].body); path[len(path)-1] != up {
		t.Errorf("the drag ends at %v, want %v", path[len(path)-1], up)
	}
}

// TestLiveDragHoldsFromEnv checks that the rest and the length of a piece
// may be set from the environment, to measure them on a real phone.
func TestLiveDragHoldsFromEnv(t *testing.T) {
	t.Setenv(liveEnvHold, "120")
	t.Setenv(liveEnvSeg, "40")
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	start := screen.Point{X: 100, Y: 1000}
	liveDragOnce(t, d, start, 50)
	acts := f.got()
	if len(acts) < 2 {
		t.Fatalf("%d pieces for a drag, want more than one", len(acts))
	}
	for i, a := range acts {
		if dur := liveDur(t, a.body); dur > 80*time.Millisecond {
			t.Errorf("piece %d takes %v on the phone, want no more than 80ms", i, dur)
		}
		if i == len(acts)-1 {
			continue
		}
		steps := liveActs(t, a.body)
		pause := steps[len(steps)-2]
		if pause["type"] != "pause" {
			t.Fatalf("piece %d lifts without a rest: %v", i, steps)
		}
		if ms, _ := pause["duration"].(float64); ms != 120 {
			t.Errorf("piece %d rests for %v ms, want 120", i, ms)
		}
	}
}

// TestLiveDragHoldEnvDefault checks that a value that is not a positive
// whole number of milliseconds leaves the default alone.
func TestLiveDragHoldEnvDefault(t *testing.T) {
	t.Setenv(liveEnvHold, "0")
	t.Setenv(liveEnvSeg, "not a number")
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	start := screen.Point{X: 100, Y: 1000}
	liveDragOnce(t, d, start, 50)
	acts := f.got()
	if len(acts) < 2 {
		t.Fatalf("%d pieces for a drag, want more than one", len(acts))
	}
	steps := liveActs(t, acts[0].body)
	pause := steps[len(steps)-2]
	if pause["type"] != "pause" {
		t.Fatalf("the first piece lifts without a rest: %v", pause)
	}
	if ms, _ := pause["duration"].(float64); ms != float64(liveHold.Milliseconds()) {
		t.Errorf("the first piece rests for %v ms, want %v", ms, liveHold.Milliseconds())
	}
}

// TestLiveDragOtherGestures checks that the gestures that do not come from
// a drag on the mirror are sent as they were: a tap is a press and a pause
// at one point, and a swipe is a move and a lift, with no rest before it.
func TestLiveDragOtherGestures(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556}
	defer d.Close()
	ctx := context.Background()
	if err := d.Tap(ctx, screen.Point{X: 300, Y: 1200}, driver.TapOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Swipe(ctx, screen.Point{X: 300, Y: 1200}, screen.Point{X: 300, Y: 1800}, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	acts := f.got()
	if len(acts) != 2 {
		t.Fatalf("%d actions for a tap and a swipe, want two", len(acts))
	}
	// The tap: down at one point, held, up.
	tap := liveActs(t, acts[0].body)
	if len(tap) != 4 || tap[0]["type"] != "pointerMove" || tap[1]["type"] != "pointerDown" ||
		tap[2]["type"] != "pause" || tap[3]["type"] != "pointerUp" {
		t.Fatalf("the tap is %v", tap)
	}
	if ms, _ := tap[2]["duration"].(float64); ms != 60 {
		t.Errorf("the tap is held for %v ms, want 60", ms)
	}
	// The swipe: down, one move, up.
	swipe := liveActs(t, acts[1].body)
	if len(swipe) != 4 || swipe[0]["type"] != "pointerMove" || swipe[1]["type"] != "pointerDown" ||
		swipe[2]["type"] != "pointerMove" || swipe[3]["type"] != "pointerUp" {
		t.Fatalf("the swipe is %v", swipe)
	}
	if ms, _ := swipe[2]["duration"].(float64); ms != 200 {
		t.Errorf("the swipe takes %v ms, want 200", ms)
	}
}

// TestLiveDragLog checks that a drag is written to the log as one line:
// how many pieces went out, how long the drag was and how far it went.
func TestLiveDragLog(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	var buf safeBuf
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556,
		log: slog.New(slog.NewTextHandler(&buf, nil))}
	defer d.Close()
	start := screen.Point{X: 100, Y: 1000}
	liveDragOnce(t, d, start, 50)
	acts := f.got()
	// The line is written when the drag is over.
	var text string
	for i := 0; i < 200; i++ {
		text = buf.String()
		if strings.Contains(text, "pieces=") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d lines in the log, want one:\n%s", len(lines), text)
	}
	line := lines[0]
	if !strings.Contains(line, "drag played on the phone") {
		t.Errorf("the line is about something else: %s", line)
	}
	pieces := logValue(t, line, "pieces")
	if n, err := strconv.Atoi(pieces); err != nil || n != len(acts) {
		t.Errorf("the line says %q pieces, the phone got %d", pieces, len(acts))
	}
	// The drag went from x=100 to x=610: 510 pixels.
	if path := logValue(t, line, "path"); path != "510" {
		t.Errorf("the line says the drag went %s pixels, want 510", path)
	}
	if dur := logValue(t, line, "drag"); dur == "" {
		t.Errorf("the line does not say how long the drag took: %s", line)
	} else if d, err := time.ParseDuration(dur); err != nil || d < 300*time.Millisecond || d > 700*time.Millisecond {
		t.Errorf("the line says the drag took %s, want about 400ms", dur)
	}
	if after := logValue(t, line, "after_lift_ms"); after == "" {
		t.Errorf("the line does not say how long the phone took after the lift: %s", line)
	}
	for _, key := range []string{"piece_max", "lag_max", "late"} {
		if logValue(t, line, key) == "" {
			t.Errorf("the line does not say %s: %s", key, line)
		}
	}
}

// TestLiveDragRealPace is the drag the real-phone bench made: one second,
// a point every 8 ms, then the lift. The played path must be the recorded
// path, no more and no less; the last request must reach the phone within
// liveTimeout of the lift; and requests must keep arriving during the drag,
// never more than 200 ms apart, so the phone follows the finger.
func TestLiveDragRealPace(t *testing.T) {
	f := &liveWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	var buf safeBuf
	d := &WDADevice{c: newWDAURL(srv.URL, ""), scale: 3, width: 1179, height: 2556,
		log: slog.New(slog.NewTextHandler(&buf, nil))}
	defer d.Close()
	ctx := context.Background()
	// 125 points, 8 px apart: 1 s of drag, and the lift stays on the screen
	// (the phone clamps points past its edge, which is not what this checks).
	start := screen.Point{X: 10, Y: 1000}
	const moves = 125
	if err := d.Touch(ctx, driver.TouchDown, start); err != nil {
		t.Fatal(err)
	}
	downAt := time.Now()
	for i := 1; i <= moves; i++ {
		if err := d.Touch(ctx, driver.TouchMove, screen.Point{X: start.X + i*8, Y: start.Y}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(8 * time.Millisecond)
	}
	up := screen.Point{X: start.X + (moves+1)*8, Y: start.Y}
	upAt := time.Now()
	if err := d.Touch(ctx, driver.TouchUp, up); err != nil {
		t.Fatal(err)
	}
	waitDrag(t, d)
	acts := f.got()
	if len(acts) == 0 {
		t.Fatal("the phone got nothing")
	}

	// The path the phone was sent, in pixels, across all pieces. Pieces are
	// head to tail, so the seams add nothing.
	var played float64
	var prev *screen.Point
	for _, a := range acts {
		for _, p := range livePath(t, a.body) {
			if prev != nil {
				played += dist(*prev, p)
			}
			p := p
			prev = &p
		}
	}
	recorded := float64((moves + 1) * 8)
	if math.Abs(played-recorded) > 1 {
		t.Errorf("the phone was sent %.0f pixels of path, the finger drew %.0f", played, recorded)
	}

	last := acts[len(acts)-1].at
	if late := last.Sub(upAt); late > liveTimeout {
		t.Errorf("the last request reached the phone %v after the lift", late)
	}

	// Requests keep arriving while the finger is down.
	prevAt := downAt
	for _, a := range acts {
		if a.at.After(upAt) {
			break
		}
		if gap := a.at.Sub(prevAt); gap > 200*time.Millisecond {
			t.Errorf("no request reached the phone for %v during the drag", gap)
		}
		prevAt = a.at
	}

	// The log says the same path.
	var text string
	for i := 0; i < 200; i++ {
		text = buf.String()
		if strings.Contains(text, "pieces=") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	line := strings.TrimSpace(text)
	if path := logValue(t, line, "path"); path != strconv.Itoa(int(math.Round(played))) {
		t.Errorf("the line says %s pixels, the phone was sent %.0f: %s", path, played, line)
	}
}
