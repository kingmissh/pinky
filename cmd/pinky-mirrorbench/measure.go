package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/DIYgod/pinky/internal/driver"
	"github.com/DIYgod/pinky/internal/driver/android/adb"
	"github.com/DIYgod/pinky/internal/driver/ios"
	"github.com/DIYgod/pinky/internal/screen"
	"github.com/DIYgod/pinky/internal/video"
)

type screenPoint = screen.Point

// stamped is a packet with the time it came off the stream.
type stamped struct {
	p  driver.Packet
	at time.Time
}

// recorder decodes a stream as the mirror does, on its own goroutine with
// the platform's hardware decoder, into planes, and keeps a record of
// every picture: when its packet came, when it was decoded, and whether it
// differs from the one before.
type recorder struct {
	s     driver.Stream
	codec driver.Codec
	pkts  chan stamped
	done  chan struct{}
	first chan struct{}

	mu        sync.Mutex
	pics      []pic
	firstPkt  time.Time
	lastMove  time.Time // the arrival of the last picture that changed
	w, h      int
	hw        string
	decode    []time.Duration
	undecoded int
	errs      int
}

func startRecorder(s driver.Stream) (*recorder, error) {
	dec, err := video.NewDecoder(s.Codec(), video.DecoderOptions{})
	if err != nil {
		return nil, err
	}
	r := &recorder{
		s:     s,
		codec: s.Codec(),
		pkts:  make(chan stamped, 4096),
		done:  make(chan struct{}),
		first: make(chan struct{}),
	}
	go r.read()
	go r.decodeLoop(dec)
	return r, nil
}

func (r *recorder) read() {
	defer close(r.pkts)
	for p := range r.s.Packets() {
		at := time.Now()
		if !p.Config {
			r.mu.Lock()
			if r.firstPkt.IsZero() {
				r.firstPkt = at
			}
			r.mu.Unlock()
		}
		r.pkts <- stamped{p, at}
	}
}

func (r *recorder) decodeLoop(dec *video.Decoder) {
	defer close(r.done)
	defer dec.Close()
	type pending struct {
		pts   time.Duration
		at    time.Time
		bytes int
	}
	var queue []pending
	var yuv video.YUV
	var prev, cur []byte
	var last video.YUV // the picture before, to tell a repeat as the mirror does
	haveLast := false
	var once sync.Once
	for sp := range r.pkts {
		if sp.p.Config {
			dec.Send(sp.p)
			continue
		}
		queue = append(queue, pending{sp.p.PTS / time.Microsecond, sp.at, len(sp.p.Data)})
		if err := dec.Send(sp.p); err != nil {
			r.mu.Lock()
			r.errs++
			r.mu.Unlock()
			continue
		}
		for {
			p, ok, err := dec.Receive()
			if err != nil {
				r.mu.Lock()
				r.errs++
				r.mu.Unlock()
				break
			}
			if !ok {
				break
			}
			if err := dec.ConvertYUV(&yuv); err != nil {
				r.mu.Lock()
				r.errs++
				r.mu.Unlock()
				continue
			}
			ready := time.Now()
			// The packet this picture came from: by its PTS, else the
			// oldest waiting. Packets before it gave no picture.
			k := 0
			for i, q := range queue {
				if q.pts == p.PTS/time.Microsecond {
					k = i
					break
				}
			}
			var q pending
			if len(queue) > 0 {
				q = queue[k]
				queue = queue[k+1:]
			} else {
				q = pending{at: ready}
			}
			const step = 4
			cur = sample(cur, yuv.Planes[0], yuv.Strides[0], yuv.Width, yuv.Height, step)
			pc := pic{at: q.at, ready: ready, bytes: q.bytes, diff: 1, repeat: haveLast && yuv.Equal(&last)}
			if !pc.repeat {
				keepPlanes(&last, &yuv)
				haveLast = true
			}
			if prev != nil {
				pc.diff = lumaDiff(prev, cur)
				pc.peak = peakDiff(prev, cur)
				n, top, bottom := movedRows(prev, cur, (yuv.Width+step-1)/step)
				if n > 0 {
					h := float64(yuv.Height)
					pc.moved, pc.top, pc.bottom = n, float64(top*step)/h, min(1, float64((bottom+1)*step)/h)
				}
			}
			prev, cur = cur, prev
			pc.changed = pc.diff > changedShare
			changed := pc.changed
			r.mu.Lock()
			r.undecoded += k
			r.pics = append(r.pics, pc)
			if changed {
				r.lastMove = q.at
			}
			r.decode = append(r.decode, ready.Sub(q.at))
			r.w, r.h = yuv.Width, yuv.Height
			if r.hw == "" {
				r.hw = "software"
				if p.Hardware {
					r.hw = dec.Hardware()
				}
			}
			r.mu.Unlock()
			once.Do(func() { close(r.first) })
		}
		if len(queue) > 64 {
			r.mu.Lock()
			r.undecoded += len(queue) - 64
			r.mu.Unlock()
			queue = queue[len(queue)-64:]
		}
	}
}

// keepPlanes copies src into dst, reusing dst's planes.
func keepPlanes(dst, src *video.YUV) {
	planes := dst.Planes
	*dst = *src
	for i, p := range src.Planes {
		dst.Planes[i] = append(planes[i][:0], p...)
	}
}

func (r *recorder) stop() {
	r.s.Close()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
	}
}

func (r *recorder) size() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.w, r.h
}

func (r *recorder) hardware() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hw
}

// between returns the pictures whose packets came from a to b.
func (r *recorder) between(a, b time.Time) []pic {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []pic
	for _, p := range r.pics {
		if !p.at.Before(a) && !p.at.After(b) {
			out = append(out, p)
		}
	}
	return out
}

func (r *recorder) since(a time.Time) []pic { return r.between(a, time.Now()) }

// waitStill waits until no picture has changed for quiet, at most max,
// and says whether it got there.
func (r *recorder) waitStill(ctx context.Context, quiet, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		last := r.lastMove
		r.mu.Unlock()
		if time.Since(last) >= quiet {
			return true
		}
		if sleep(ctx, 50*time.Millisecond) != nil {
			return false
		}
	}
	return false
}

func (r *recorder) decodeStats() (med, max float64, undecoded, errs int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.decode) > 0 {
		med = ms(median(r.decode))
		for _, d := range r.decode {
			max = maxf(max, ms(d))
		}
	}
	return med, max, r.undecoded, r.errs
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// firstResult times a stream's start from the Stream call.
type firstResult struct {
	call, packet, picture time.Duration
}

// firstPicture asks for the stream and waits for its first picture.
func firstPicture(ctx context.Context, dev driver.Device) (firstResult, *recorder, error) {
	var f firstResult
	t0 := time.Now()
	s, err := dev.Stream(ctx, driver.StreamOptions{})
	if err != nil {
		return f, nil, err
	}
	f.call = time.Since(t0)
	r, err := startRecorder(s)
	if err != nil {
		s.Close()
		return f, nil, err
	}
	select {
	case <-r.first:
	case <-r.done:
		return f, nil, fmt.Errorf("the stream ended before its first picture: %v", s.Err())
	case <-time.After(20 * time.Second):
		r.stop()
		return f, nil, errors.New("no picture in 20 s")
	case <-ctx.Done():
		r.stop()
		return f, nil, ctx.Err()
	}
	r.mu.Lock()
	f.packet = r.firstPkt.Sub(t0)
	f.picture = r.pics[0].ready.Sub(t0)
	r.mu.Unlock()
	return f, r, nil
}

// useSwipe says whether a drag should move the list with WebDriverAgent's
// swipe rather than with raw touches. A WebDriverAgent device swipes by
// default: the helper plays its raw touches back when the finger lifts,
// so timing them would time nothing. With rawTouch the raw touches are
// measured instead, as the mirror sends them.
func useSwipe(dev driver.Device, rawTouch bool) bool {
	if rawTouch {
		return false
	}
	_, ok := dev.(*ios.WDADevice)
	return ok
}

// touchRoute names the way the drags move the list, so that every number
// says which path it came from: the raw touches of the mirror, or
// WebDriverAgent's swipe.
func touchRoute(dev driver.Device, rawTouch bool) string {
	if useSwipe(dev, rawTouch) {
		return "wda swipe"
	}
	return "raw touch"
}

// drag scrolls the list with raw touches in the stream's pixels, as the
// person does on the mirror: down at three quarters of the height, a
// jump past the touch slop, then moves every 8 ms at an even speed to
// three tenths of it (or the other way), a hold so the list does not fling,
// and up. A WebDriverAgent device, whose raw touches are replayed at the
// end, swipes instead, unless rawTouch asks for the raw touches.
func drag(ctx context.Context, dev driver.Device, rec *recorder, gfx *gfxInfo, up bool, dur time.Duration, rawTouch bool) (trial, error) {
	if useSwipe(dev, rawTouch) {
		// useSwipe only says true for a WebDriverAgent device.
		w, _ := dev.(*ios.WDADevice)
		return swipe(ctx, w, rec, up, dur)
	}
	t, ok := dev.(driver.Toucher)
	if !ok {
		return trial{}, fmt.Errorf("%T has no raw touches", dev)
	}
	w, h := rec.size()
	x := w / 2
	y0, y1 := h*3/4, h*3/10
	if !up {
		y0, y1 = y1, y0
	}
	dir := 1
	if y1 < y0 {
		dir = -1
	}
	// The app's frames count from the reset to the end of the move, each
	// taken halfway through its adb call, as that is about when it ran on
	// the phone.
	r0 := time.Now()
	gfx.reset(ctx)
	from := r0.Add(time.Since(r0) / 2)
	down := time.Now()
	if err := t.Touch(ctx, driver.TouchDown, screenPoint{X: x, Y: y0}); err != nil {
		return trial{}, err
	}
	lifted := false
	defer func() {
		if !lifted {
			t.Touch(context.Background(), driver.TouchUp, screenPoint{X: x, Y: y0})
		}
	}()
	if err := sleep(ctx, 20*time.Millisecond); err != nil {
		return trial{}, err
	}
	jump := y0 + dir*h*4/100
	move := time.Now()
	if err := t.Touch(ctx, driver.TouchMove, screenPoint{X: x, Y: jump}); err != nil {
		return trial{}, err
	}
	y := jump
	for {
		if err := sleep(ctx, 8*time.Millisecond); err != nil {
			return trial{}, err
		}
		frac := min(1, float64(time.Since(move))/float64(dur))
		y = jump + int(float64(y1-jump)*frac)
		if err := t.Touch(ctx, driver.TouchMove, screenPoint{X: x, Y: y}); err != nil {
			return trial{}, err
		}
		if frac >= 1 {
			break
		}
	}
	end := time.Now()
	app, appAt := -1, time.Duration(0)
	if gfx != nil {
		c0 := time.Now()
		app = gfx.frames(ctx)
		appAt = c0.Add(time.Since(c0) / 2).Sub(from)
	}
	if err := sleep(ctx, 200*time.Millisecond-time.Since(end)); err != nil {
		return trial{}, err
	}
	err := t.Touch(ctx, driver.TouchUp, screenPoint{X: x, Y: y})
	lifted = true
	if err != nil {
		return trial{}, err
	}
	if err := sleep(ctx, 800*time.Millisecond); err != nil {
		return trial{}, err
	}
	tr := trial{move: move, end: end, pics: rec.between(down, time.Now()), app: app, appAt: appAt}
	return tr, nil
}

var wdaSize screenPoint

// swipe drags the list with WebDriverAgent's swipe, in screenshot pixels,
// timed from the call.
func swipe(ctx context.Context, d *ios.WDADevice, rec *recorder, up bool, dur time.Duration) (trial, error) {
	if wdaSize.X == 0 {
		img, err := d.Screenshot(ctx)
		if err != nil {
			return trial{}, err
		}
		wdaSize = screenPoint{X: img.Bounds().Dx(), Y: img.Bounds().Dy()}
	}
	w, h := wdaSize.X, wdaSize.Y
	from, to := screenPoint{X: w / 2, Y: h * 3 / 4}, screenPoint{X: w / 2, Y: h * 3 / 10}
	if !up {
		from, to = to, from
	}
	move := time.Now()
	if err := d.Swipe(ctx, from, to, dur); err != nil {
		return trial{}, err
	}
	end := time.Now()
	if err := sleep(ctx, time.Second); err != nil {
		return trial{}, err
	}
	return trial{move: move, end: end, pics: rec.between(move.Add(-100*time.Millisecond), time.Now()), app: -1}, nil
}

// gfxInfo counts the frames an Android app draws, from dumpsys gfxinfo.
type gfxInfo struct {
	client      *adb.Client
	serial, pkg string
}

func (g *gfxInfo) reset(ctx context.Context) {
	if g == nil {
		return
	}
	g.client.Shell(ctx, g.serial, "dumpsys gfxinfo "+g.pkg+" reset")
}

var totalFrames = regexp.MustCompile(`Total frames rendered: (\d+)`)

func (g *gfxInfo) frames(ctx context.Context) int {
	if g == nil {
		return -1
	}
	out, err := g.client.Shell(ctx, g.serial, "dumpsys gfxinfo "+g.pkg)
	if err != nil {
		return -1
	}
	m := totalFrames.FindSubmatch(out)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return -1
	}
	return n
}
