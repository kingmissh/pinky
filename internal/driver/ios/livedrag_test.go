package ios

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
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
