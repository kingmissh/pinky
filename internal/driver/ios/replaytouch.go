package ios

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	// liveTimeout bounds one segment's request, so a phone that stops
	// answering does not keep the player waiting.
	liveTimeout = 10 * time.Second
)

// liveSeg is a piece of the recorded path to play on the phone.
type liveSeg struct {
	pts   []screen.Point
	steps []time.Duration // steps[i]: how long the move to pts[i] took
	end   time.Time       // when the finger was at the last point of the piece
	gen   int             // which press the piece belongs to
}

// liveState is what the player should do next.
type liveState int

const (
	liveWait liveState = iota // the piece is not ready: wait for the path
	liveIdle                  // the drag is over: wait for the next one
	livePlay                  // play this piece
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
			return liveSeg{}, liveIdle
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
	return liveSeg{pts: pts, steps: steps, end: r.at[j], gen: r.gen}, livePlay
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

// playLive plays the recorded drag on the phone, piece by piece, until the
// finger lifts and everything recorded has been played.
func (d *WDADevice) playLive(ctx context.Context) {
	for {
		s, st := d.rec.next(liveSegMax, liveRest)
		switch st {
		case liveIdle:
			// The drag is over: wait for the next one.
			select {
			case <-d.live.wake:
			case <-d.live.stop:
				return
			case <-ctx.Done():
				return
			}
		case liveWait:
			// The piece is not ready: wait for more of the path, for the
			// finger to rest, or for the device to close.
			select {
			case <-d.live.wake:
			case <-d.live.stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(liveRest):
			}
		case livePlay:
			if err := d.sendSeg(ctx, s); err != nil {
				// The phone may be gone: the drag ends here, and the error
				// is told on the next touch.
				d.live.fail(err)
				d.rec.fail()
			}
		}
	}
}

// sendSeg plays one piece of the path on the phone. The request is the
// device's, not the person's touch: it outlives the touch that recorded
// the piece.
func (d *WDADevice) sendSeg(ctx context.Context, s liveSeg) error {
	path := make([]wdaPoint, len(s.pts))
	for i, q := range s.pts {
		path[i] = d.toPoints(q)
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()
	if err := d.c.gesturePath(rctx, path, s.steps, 0); err != nil {
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
