package ios

import (
	"context"
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
type touchRecorder struct {
	mu    sync.Mutex
	down  bool
	start time.Time
	pts   []screen.Point
	at    []time.Time
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

// recordedGesture is a finished touch.
type recordedGesture struct {
	pts   []screen.Point
	steps []time.Duration // steps[i]: how long the move to pts[i] took
	held  time.Duration
	tap   bool // the finger did not travel
}

// touch records one raw touch; on TouchUp it returns the gesture to
// perform.
func (r *touchRecorder) touch(a driver.TouchAction, p screen.Point, now time.Time) (*recordedGesture, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch a {
	case driver.TouchDown:
		r.down, r.start = true, now
		r.pts, r.at = []screen.Point{p}, []time.Time{now}
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
			// Keep the shape: drop every other point but the first.
			kp, ka := r.pts[:1], r.at[:1]
			for i := 2; i < len(r.pts); i += 2 {
				kp, ka = append(kp, r.pts[i]), append(ka, r.at[i])
			}
			r.pts, r.at = kp, ka
		}
		r.pts, r.at = append(r.pts, p), append(r.at, now)
		return nil, nil
	case driver.TouchUp:
		if !r.down {
			return nil, nil
		}
		r.down = false
		pts, at := append(r.pts, p), append(r.at, now)
		g := &recordedGesture{held: now.Sub(r.start)}
		far := 0.0
		for _, q := range pts {
			far = math.Max(far, dist(pts[0], q))
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
		return g, nil
	}
	return nil, fmt.Errorf("touch action %q: %w", a, driver.ErrUnsupported)
}

func dist(a, b screen.Point) float64 {
	return math.Hypot(float64(a.X-b.X), float64(a.Y-b.Y))
}

// Touch implements driver.Toucher for the mirror: the touch plays back when
// the finger lifts (see touchRecorder).
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
	if err != nil || g == nil {
		return err
	}
	if g.tap {
		return d.Tap(ctx, g.pts[0], driver.TapOptions{LongPress: g.held > recLongPress})
	}
	path := make([]wdaPoint, len(g.pts))
	for i, q := range g.pts {
		path[i] = d.toPoints(q)
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	return d.c.gesturePath(ctx, path, g.steps, 0)
}

var _ driver.Toucher = (*WDADevice)(nil)
