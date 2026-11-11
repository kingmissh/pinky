package ios

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DIYgod/pinky/internal/driver"
	"github.com/DIYgod/pinky/internal/screen"
)

// touchRecorder turns the mirror's raw touches into whole gestures, for
// backends that can only perform a gesture from start to end (WebDriverAgent
// performs W3C actions). It records the finger's path while it is down and
// plays it back when it lifts: a tap, a long press, or a drag along the
// recorded path at its own pace. Points arrive in the stream's picture and
// are converted to screen pixels, since the picture may be smaller.
//
// A drag is also played while the finger is down (see liveDrag): the path
// is cut into segments, and each segment goes to the phone as soon as it is
// recorded, so the phone follows the finger instead of waiting for the
// lift.
type touchRecorder struct {
	mu     sync.Mutex
	down   bool
	start  time.Time
	origin screen.Point // where the finger went down
	pts    []screen.Point
	at     []time.Time

	sentAt time.Time // when the finger was at the last point played on the phone
	far    float64   // how far the finger went from where it went down
	live   bool      // the finger went past the tap slop: a drag, played as it goes
	up     bool      // the finger lifted
	failed bool      // a segment failed: the rest of the path does not go out
	gen    int       // which press the path belongs to
}

// Recording limits: points closer than minMove or sooner than minGap after
// the last one are dropped, and a path keeps at most maxPoints.
const (
	recMinMove   = 4
	recMinGap    = 16 * time.Millisecond
	recMaxPoints = 200
	recTapSlop   = 24 // pixels a tap may wander
	recLongPress = 500 * time.Millisecond
)

func dist(a, b screen.Point) float64 {
	return math.Hypot(float64(a.X-b.X), float64(a.Y-b.Y))
}

// recordedGesture is a finished touch.
type recordedGesture struct {
	pts   []screen.Point
	steps []time.Duration // steps[i]: how long the move to pts[i] took
	held  time.Duration
	tap   bool // the finger did not travel
	live  bool // the drag is played on the phone while the finger is down
}

// touch records one raw touch; on TouchUp it returns the gesture to
// perform. A move that takes the finger past the tap slop returns a gesture
// with live set: the drag is then played on the phone while the finger is
// down (see liveDrag).
func (r *touchRecorder) touch(a driver.TouchAction, p screen.Point, now time.Time) (*recordedGesture, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch a {
	case driver.TouchDown:
		r.down, r.start = true, now
		r.origin = p
		r.pts, r.at = []screen.Point{p}, []time.Time{now}
		r.sentAt, r.far, r.live, r.up, r.failed = time.Time{}, 0, false, false, false
		r.gen++
		return nil, nil
	case driver.TouchMove:
		if !r.down {
			return nil, nil
		}
		last := r.pts[len(r.pts)-1]
		if dist(last, p) < recMinMove || now.Sub(r.at[len(r.at)-1]) < recMinGap {
			return nil, nil
		}
		if len(r.pts) >= recMaxPoints {
			// Keep the shape: drop every other point but the first. A piece
			// on its way to the phone is a copy, and played finds its place
			// in time, so neither is disturbed by this.
			kp, ka := r.pts[:1], r.at[:1]
			for i := 2; i < len(r.pts); i += 2 {
				kp, ka = append(kp, r.pts[i]), append(ka, r.at[i])
			}
			r.pts, r.at = kp, ka
		}
		r.pts, r.at = append(r.pts, p), append(r.at, now)
		if d := dist(r.origin, p); d > r.far {
			r.far = d
		}
		if !r.live && r.far >= recTapSlop {
			// The finger has gone past the slop: this is a drag, and it is
			// played on the phone while the finger is down.
			r.live = true
			return &recordedGesture{live: true}, nil
		}
		return nil, nil
	case driver.TouchUp:
		if !r.down {
			return nil, nil
		}
		r.down = false
		// The lift is recorded even when the clock has not moved since the
		// last move: the clock is coarse, and two points with the same
		// time leave the player thinking the phone's finger has not moved,
		// so it plays the same piece again and again.
		if n := len(r.at); n > 0 && !now.After(r.at[n-1]) {
			now = r.at[n-1].Add(time.Nanosecond)
		}
		pts, at := append(r.pts, p), append(r.at, now)
		g := &recordedGesture{held: now.Sub(r.start)}
		far := r.far
		if d := dist(r.origin, p); d > far {
			far = d
		}
		if far < recTapSlop {
			g.tap = true
			g.pts = pts[:1]
			g.steps = []time.Duration{0}
			return g, nil
		}
		g.pts = pts
		g.steps = make([]time.Duration, len(pts))
		for i := 1; i < len(pts); i++ {
			g.steps[i] = at[i].Sub(at[i-1])
		}
		if r.live {
			// The player takes the rest of the path, the point the finger
			// was lifted at included.
			r.pts, r.at = pts, at
			r.up = true
			g.live = true
		}
		return g, nil
	}
	return nil, fmt.Errorf("touch action %q: %w", a, driver.ErrUnsupported)
}

// A drag on the phone is played in segments: WebDriverAgent performs a whole
// gesture before it answers, so the drag follows the finger a little behind.
const (
	// liveSegMax is how long a segment may take on the phone: the phone
	// plays a segment before it answers, so a long one keeps the phone
	// behind the finger.
	liveSegMax = 120 * time.Millisecond
	// liveRest is how long the finger may rest before the path recorded
	// so far is played, so a slow drag is not held back.
	liveRest = 60 * time.Millisecond
	// liveHold is how long the finger rests at the end of a segment before
	// it lifts. A segment that lifts while the finger is moving looks to
	// the phone like a flick: the content keeps sliding after the finger
	// is gone, and a drag that is cut into segments flings the content
	// again and again. A rest first makes the lift happen at a standstill,
	// so the content stops where the finger stopped.
	//
	// Only the last segment of a drag lifts this way: that lift is the
	// person's own, and it keeps its speed, so the content slides on a
	// little, as it does on a real phone.
	liveHold = 50 * time.Millisecond
	// liveTimeout bounds one segment's request, so a phone that stops
	// answering does not keep the player waiting.
	liveTimeout = 10 * time.Second
	// liveLagWarn is how far past the time a segment asked for the phone may
	// take before the log calls the drag abnormal. A request has to travel to
	// the phone and back, which is some tens of milliseconds; much more than
	// that means the phone is not keeping up with the finger.
	liveLagWarn = 100 * time.Millisecond
)

// liveSeg is a piece of the recorded path to play on the phone.
type liveSeg struct {
	pts   []screen.Point
	steps []time.Duration // steps[i]: how long the move to pts[i] took
	end   time.Time       // when the finger was at the last point of the piece
	gen   int             // which press the piece belongs to
	last  bool            // the piece ends the drag: it holds the point the finger lifted at
}

// liveState is what the player should do next.
type liveState int

const (
	liveWait liveState = iota // the piece is not ready: wait for the path
	liveIdle                  // there is no drag to play: wait for the next one
	livePlay                  // play this piece
	liveDone                  // the drag is over: everything recorded has been played
)

// next gives the player the next piece of the path to play on the phone:
// the points after the one played last (the piece starts at the point
// played last, so the phone's finger does not jump), cut so that the piece
// takes no longer than max.
//
// A piece that is not full waits for the finger to rest, so a slow drag is
// not held back, and what is left is played when the finger lifts.
func (r *touchRecorder) next(max, idle time.Duration) (liveSeg, liveState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.live || r.failed {
		// No drag, or a segment failed: the rest of the path does not go
		// out.
		return liveSeg{}, liveIdle
	}
	last := len(r.pts) - 1
	from := r.from()
	if from >= last {
		if r.up {
			// The finger lifted and everything recorded has been played:
			// the drag is over.
			return liveSeg{}, liveDone
		}
		return liveSeg{}, liveWait
	}
	// The piece: from the point played last to the end of what is
	// recorded, cut so that it takes no longer than max. A move that took
	// longer than that (the finger rested) is shortened, so the phone does
	// not play slower than the finger.
	dur := time.Duration(0)
	j := from
	for j < last {
		step := r.at[j+1].Sub(r.at[j])
		if step > max {
			step = max
		}
		dur += step
		j++
		if dur >= max {
			break
		}
	}
	if !r.up && dur < max && time.Since(r.at[last]) < idle {
		// The piece is not full and the finger is still moving: wait.
		return liveSeg{}, liveWait
	}
	pts := make([]screen.Point, j-from+1)
	copy(pts, r.pts[from:j+1])
	steps := make([]time.Duration, len(pts))
	for i := 1; i < len(pts); i++ {
		steps[i] = r.at[from+i].Sub(r.at[from+i-1])
		if steps[i] > max {
			steps[i] = max
		}
	}
	if !r.sentAt.IsZero() && !r.at[j].After(r.sentAt) {
		// The piece ends where the phone's finger already is: it would be
		// played again and again. The drag ends here.
		return liveSeg{}, liveDone
	}
	// The last piece is the one that ends at the point the finger lifted
	// at: that lift is the person's own and keeps its speed. Every other
	// piece lifts in the middle of the drag, and rests first.
	return liveSeg{pts: pts, steps: steps, end: r.at[j], gen: r.gen, last: r.up && j == last}, livePlay
}

// dragTimes returns when the finger went down and when it lifted: the two
// ends of the drag that is being played. They are zero when no drag has
// been played.
func (r *touchRecorder) dragTimes() (time.Time, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.up || len(r.at) == 0 {
		return time.Time{}, time.Time{}
	}
	return r.start, r.at[len(r.at)-1]
}

// from is the place in the path the phone's finger has reached: the first
// point at or after the one played last, or the first point when nothing
// has been played. It is found by time, since the path may have lost
// points while a piece was on its way.
func (r *touchRecorder) from() int {
	if r.sentAt.IsZero() {
		return 0
	}
	i := 0
	for i < len(r.at)-1 && r.at[i].Before(r.sentAt) {
		i++
	}
	return i
}

// played marks the points of a piece as played on the phone: the phone's
// finger is at the last point of the piece, the one recorded at end. A
// piece of an earlier press is ignored: the finger is down again by then.
func (r *touchRecorder) played(gen int, end time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if gen != r.gen {
		return
	}
	if end.After(r.sentAt) {
		r.sentAt = end
	}
}

// fail marks that a segment failed: the rest of the path does not go out.
func (r *touchRecorder) fail() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = true
}

// dragDone reports whether the drag is over: the finger lifted and
// everything recorded has been played.
func (r *touchRecorder) dragDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.up && (r.failed || !r.sentAt.Before(r.at[len(r.at)-1]))
}

// liveDrag plays the mirror's drag on the phone while the finger is down:
// the recorded path is cut into segments, and each segment goes to the
// phone as soon as it is recorded. WebDriverAgent performs a whole gesture
// before it answers, so one segment is on the phone at a time: the drag
// follows the finger a little behind, but the phone moves while the finger
// is down, instead of after it lifts.
type liveDrag struct {
	mu        sync.Mutex
	once      sync.Once
	closeOnce sync.Once
	wake      chan struct{} // the player waits on it for more of the path
	stop      chan struct{} // closed when the device closes
	cancel    context.CancelFunc
	running   bool  // the player is running
	err       error // a segment that failed, for the next touch
	segMax    time.Duration
	hold      time.Duration
	tuned     bool // the calibration was read
}

// The calibration of the live drag may be set from the environment, to
// measure it on a real phone: PINKY_LIVEDRAG_SEG is the length of a piece
// in milliseconds, and PINKY_LIVEDRAG_HOLD is the rest before the finger
// lifts. It is a temporary knob: a value that is not a positive whole
// number of milliseconds is ignored and the default is used.
const (
	liveEnvSeg  = "PINKY_LIVEDRAG_SEG"
	liveEnvHold = "PINKY_LIVEDRAG_HOLD"
)

// tune sets how the live drag is cut on this device: how long a piece may
// take on the phone, and how long the finger rests at the end of a piece
// before it lifts. A hold of zero means the finger lifts as soon as the
// piece ends, as it did before the rest was added.
func (l *liveDrag) tune(seg, hold time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.segMax, l.hold, l.tuned = seg, hold, true
}

// cutting returns how the live drag is cut: how long a piece may take on
// the phone, and how long the finger rests before a piece lifts.
func (l *liveDrag) cutting() (time.Duration, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.tuned {
		return liveSegMax, liveHold
	}
	return l.segMax, l.hold
}

// tuneFromEnv reads the calibration from the environment, once, when the
// player starts.
func (l *liveDrag) tuneFromEnv() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tuned {
		return
	}
	l.tuned = true
	l.segMax, l.hold = liveSegMax, liveHold
	if ms, ok := liveEnvMS(liveEnvSeg); ok {
		l.segMax = ms
	}
	if ms, ok := liveEnvMS(liveEnvHold); ok {
		l.hold = ms
	}
}

// liveEnvMS reads a length in whole milliseconds from the environment. It
// gives nothing when the variable is not there, or when it does not name a
// positive whole number of milliseconds: the default is used then.
func liveEnvMS(name string) (time.Duration, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return time.Duration(n) * time.Millisecond, true
}

// init makes the channels, once.
func (l *liveDrag) init() {
	l.once.Do(func() {
		l.wake = make(chan struct{}, 1)
		l.stop = make(chan struct{})
	})
}

// wakeUp tells the player that there is more of the path.
func (l *liveDrag) wakeUp() {
	l.init()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// fail keeps the failure of a segment, to be told once, on the next touch.
func (l *liveDrag) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		l.err = err
	}
}

// takeErr gives the failure of a segment, once.
func (l *liveDrag) takeErr() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.err
	l.err = nil
	return err
}

// close ends the player.
func (l *liveDrag) close() {
	l.init()
	l.closeOnce.Do(func() {
		close(l.stop)
		l.mu.Lock()
		if l.cancel != nil {
			l.cancel()
		}
		l.mu.Unlock()
	})
}

// startLive starts the player for this device, if it is not running yet.
// It plays the drags of the mirror until the device closes.
func (d *WDADevice) startLive() {
	d.live.init()
	d.live.tuneFromEnv()
	d.live.mu.Lock()
	if d.live.running {
		d.live.mu.Unlock()
		return
	}
	d.live.running = true
	ctx, cancel := context.WithCancel(context.Background())
	d.live.cancel = cancel
	d.live.mu.Unlock()
	go func() {
		defer func() {
			d.live.mu.Lock()
			d.live.running = false
			d.live.mu.Unlock()
		}()
		d.playLive(ctx)
	}()
}

// liveStats is how a drag was played on the phone, for the log.
type liveStats struct {
	gen      int           // which press the drag belongs to
	segs     int           // pieces that went out
	pathLen  float64       // how far the phone's finger went, in pixels
	over     int           // pieces that took longer on the phone than the bound
	maxPiece time.Duration // the longest piece on the phone
	maxLag   time.Duration // the longest the phone took beyond a piece's own time
	lastAt   time.Time     // when the last request ended
	logged   bool          // the drag has been to the log
}

// playLive plays the recorded drag on the phone, piece by piece, until the
// finger lifts and everything recorded has been played.
func (d *WDADevice) playLive(ctx context.Context) {
	var st liveStats
	for {
		segMax, _ := d.live.cutting()
		s, state := d.rec.next(segMax, liveRest)
		switch state {
		case liveIdle:
			// There is no drag to play: wait for the next one.
			if !d.waitLive(ctx, 0) {
				return
			}
		case liveWait:
			// The piece is not ready: wait for more of the path, for the
			// finger to rest, or for the device to close.
			if !d.waitLive(ctx, liveRest) {
				return
			}
		case livePlay:
			if st.gen != s.gen {
				st = liveStats{gen: s.gen}
			}
			started := time.Now()
			if err := d.sendSeg(ctx, s); err != nil {
				// The phone may be gone: the drag ends here, and the error
				// is told on the next touch.
				d.live.fail(err)
				d.rec.fail()
				if !st.logged {
					st.logged = true
					d.noteDrag(st, err)
				}
				continue
			}
			st.segs++
			st.pathLen += d.sentLen(s.pts)
			own := liveSegDur(s.steps)
			if !s.last {
				// A piece that is not the last waits at its end before it
				// lifts, and that wait is part of how long the phone spends
				// on it.
				_, hold := d.live.cutting()
				own += hold
			}
			if own > st.maxPiece {
				st.maxPiece = own
			}
			lag := time.Since(started) - own
			if lag > st.maxLag {
				st.maxLag = lag
			}
			if lag > liveLagWarn {
				// The phone took far longer than the piece asked for: the
				// drag fell behind the finger here.
				st.over++
			}
			st.lastAt = time.Now()
		case liveDone:
			// The drag is over: it goes to the log, once.
			if !st.logged {
				st.logged = true
				d.noteDrag(st, nil)
			}
			if !d.waitLive(ctx, 0) {
				return
			}
		}
	}
}

// waitLive waits for the next of the path, for the device to close, or for
// the player to be stopped. It waits for the rest when timeout is not zero.
// It says whether the player goes on.
func (d *WDADevice) waitLive(ctx context.Context, timeout time.Duration) bool {
	if timeout <= 0 {
		select {
		case <-d.live.wake:
		case <-d.live.stop:
			return false
		case <-ctx.Done():
			return false
		}
		return true
	}
	select {
	case <-d.live.wake:
	case <-d.live.stop:
		return false
	case <-ctx.Done():
		return false
	case <-time.After(timeout):
	}
	return true
}

// sentLen is how far the finger went on the phone for a piece, in screen
// pixels: the path as it was sent, so a point the phone could not take (the
// edge of the screen) does not count.
func (d *WDADevice) sentLen(pts []screen.Point) float64 {
	var l float64
	for i := 1; i < len(pts); i++ {
		a, b := d.toPoints(pts[i-1]), d.toPoints(pts[i])
		l += math.Hypot(b.X-a.X, b.Y-a.Y) * d.scale
	}
	return l
}

// liveSegDur is how long a piece takes on the phone: the sum of the times
// its moves are given.
func liveSegDur(steps []time.Duration) time.Duration {
	var t time.Duration
	for _, s := range steps {
		t += s
	}
	return t
}

// noteDrag logs how a drag was played on the phone: how many pieces went
// out, how long the drag was, how far it went, how long the phone took to
// finish the last piece after the finger lifted, and whether any piece took
// far longer on the phone than it asked for. It is one line for a drag: the
// person reads pinky.log, and a line for every piece would drown it.
func (d *WDADevice) noteDrag(st liveStats, err error) {
	if d.log == nil || (st.segs == 0 && err == nil) {
		return
	}
	down, up := d.rec.dragTimes()
	var after int64
	if !st.lastAt.IsZero() && !up.IsZero() {
		after = st.lastAt.Sub(up).Milliseconds()
		if after < 0 {
			after = 0
		}
	}
	args := []any{
		"device", d.info.ID,
		"pieces", st.segs,
		"drag", up.Sub(down).Round(time.Millisecond),
		"path", int(math.Round(st.pathLen)),
		"after_lift_ms", after,
		"piece_max", st.maxPiece.Round(time.Millisecond),
		"lag_max", st.maxLag.Round(time.Millisecond),
		"late", st.over,
	}
	if err != nil {
		args = append(args, "error", err)
	}
	d.log.Info("iphone: drag played on the phone", args...)
}

// sendSeg plays one piece of the path on the phone. The request is the
// device's, not the person's touch: it outlives the touch that recorded
// the piece.
func (d *WDADevice) sendSeg(ctx context.Context, s liveSeg) error {
	path := make([]wdaPoint, len(s.pts))
	for i, q := range s.pts {
		path[i] = d.toPoints(q)
	}
	// The finger rests at the end of the piece before it lifts, unless the
	// piece ends the drag: that lift is the person's own and keeps its
	// speed, so the content slides on as it does on a real phone.
	_, hold := d.live.cutting()
	rest := hold
	if s.last {
		rest = 0
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()
	if err := d.c.gesturePathRest(rctx, path, s.steps, 0, rest); err != nil {
		return err
	}
	d.rec.played(s.gen, s.end)
	return nil
}

// Touch implements driver.Toucher for the mirror: the touch is recorded and
// played back on the phone. A tap is played when the finger lifts, as
// before; a drag is played while the finger is down, in pieces (see
// liveDrag), so the phone follows the finger.
func (d *WDADevice) Touch(ctx context.Context, a driver.TouchAction, p screen.Point) error {
	// The picture is upright, so its shape says the orientation the
	// person saw when they touched it.
	vw, vh := d.videoSize()
	if vw > 0 && vh > 0 {
		d.seeOrientation(float64(vw), float64(vh))
	}
	w, h := d.size()
	p = fromVideo(p, vw, vh, w, h)
	g, err := d.rec.touch(a, p, time.Now())
	if err != nil {
		return err
	}
	// The player takes the rest of the path: it needs to know about it.
	d.live.wakeUp()
	if g != nil && g.live {
		d.startLive()
	}
	// A segment that failed: the error is told once, on the next touch,
	// and the touch after that works again.
	failed := d.live.takeErr()
	switch {
	case g == nil:
		return failed
	case g.tap:
		err := d.Tap(ctx, g.pts[0], driver.TapOptions{LongPress: g.held > recLongPress})
		if failed != nil {
			return errors.Join(failed, err)
		}
		return err
	case g.live:
		// The player plays the rest of the path.
		return failed
	}
	// A drag that never went past the slop: it is played in one go, as it
	// was before.
	path := make([]wdaPoint, len(g.pts))
	for i, q := range g.pts {
		path[i] = d.toPoints(q)
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	return d.c.gesturePath(ctx, path, g.steps, 0)
}

var _ driver.Toucher = (*WDADevice)(nil)
