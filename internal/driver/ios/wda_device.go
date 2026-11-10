package ios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DIYgod/pinky/internal/driver"
	"github.com/DIYgod/pinky/internal/screen"
	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/instruments"
)

// WDADevice drives an iPhone through the WebDriverAgent helper: iOS 17.4 to
// 26, where the phone refuses CoreDevice's screen stream and HID input.
// Coordinates are screenshot pixels, as on the other paths; WDA works in
// points, so every point is divided by the screen's scale on the way in and
// multiplied on the way out.
type WDADevice struct {
	info  driver.Info
	c     *wdaClient
	entry goios.DeviceEntry // the usbmuxd entry; zero for a simulator
	run   *wdaRun           // the test run hosting WDA, when Pinky started it
	l     *link             // the tunnel the run goes through, from iOS 17.4

	scale float64
	// width and height are the screen's size in pixels when the phone was
	// opened, in the orientation it had then; size gives it the right way
	// up for the orientation last seen.
	width, height int
	landscape     atomic.Int32 // orientation as last seen: 0 not yet, 1 portrait, 2 landscape
	ui            atomic.Int32 // the interface's turn from GET /rotation, plus one; 0 not read yet
	uiAt          atomic.Int64 // when ui was read, unix nanos

	inputMu sync.Mutex // one gesture or text entry at a time
	appMu   sync.Mutex
	apps    *appCatalog
	// schemesFn, on a simulator, lists the apps by URL scheme (SetLinkSchemes).
	schemesFn func(context.Context) (map[string][]driver.App, error)
	rec       touchRecorder
	rec2      pinchRecorder // the mirror's two-finger touches (pinch.go)
	live      liveDrag      // the mirror's drag, played while the finger is down (replaytouch.go)

	videoMu sync.Mutex
	video   driver.Stream // the last stream, whose picture the mirror's touches are in

	closeOnce sync.Once

	logs    appLogs // the apps whose logs a task reads (logs.go)
	procMu  sync.Mutex
	procSvc *instruments.DeviceInfoService // lists processes for logs

	imageDir string // Pinky's folder for developer disk images (checkup.go)

	log *slog.Logger // slow reads, with what took the time; nil: none

	// appsFn lists a Simulator's apps (simctl), which has no installation
	// proxy; nil on a phone.
	appsFn func(context.Context) ([]driver.App, error)

	helperFn   func(context.Context) (time.Time, error) // for tests: helperExpires
	lockdownFn func(context.Context) (bool, error)      // for tests: lockdownLocked
}

// openWDA connects to a WDA server that is running and reads the screen's
// size. run, when not nil, is the test run that hosts it and is stopped on
// Close.
func openWDA(ctx context.Context, info driver.Info, c *wdaClient, entry goios.DeviceEntry, run *wdaRun) (*WDADevice, error) {
	st, err := c.waitReady(ctx)
	if err != nil {
		return nil, err
	}
	scr, err := c.screenInfo(ctx)
	if err != nil {
		return nil, err
	}
	d := &WDADevice{
		info: info, c: c, entry: entry, run: run,
		scale:  scr.Scale,
		width:  int(math.Round(scr.ScreenSize.Width * scr.Scale)),
		height: int(math.Round(scr.ScreenSize.Height * scr.Scale)),
	}
	// Screenshots are the coordinate space, and XCTest's can be a pixel
	// narrower than points times scale (1178 for 393 points at 3x).
	if b, err := c.screenshot(ctx); err == nil {
		if cfg, err := png.DecodeConfig(bytes.NewReader(b)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
			d.width, d.height = cfg.Width, cfg.Height
		}
	}
	d.info.Backend = backendWDA
	if d.info.OSVersion == "" {
		d.info.OSVersion = st.OS.Version
	}
	c.gone = d.gone
	return d, nil
}

// gone wraps err, a request that failed to reach WDA, as driver.ErrOffline
// once WDA is gone, so the phone does not stay open with every call
// failing: the gateway reads driver.ErrOffline as the phone having dropped,
// and opens it again, which starts the runner anew. A runner Pinky started
// is gone when its test ended (testmanagerd ends it when the runner dies)
// or the tunnel stopped; while it runs, a failed request is only that. A
// runner Pinky did not start (Xcode's) gives no such sign: it is gone when
// its port no longer takes connections.
func (d *WDADevice) gone(err error) error {
	if err == nil || errors.Is(err, driver.ErrOffline) {
		return err
	}
	var de *wdaDialError
	if d.ended() || (d.run == nil && errors.As(err, &de)) {
		return fmt.Errorf("%v: %w", err, driver.ErrOffline)
	}
	return err
}

// ended reports whether the runner's test Pinky started has ended or the
// tunnel it went through stopped.
func (d *WDADevice) ended() bool {
	if d.run != nil {
		if alive, _ := d.run.alive(); !alive {
			return true
		}
	}
	if d.l != nil && d.l.tunnel != nil {
		select {
		case <-d.l.tunnel.Done():
			return true
		default:
		}
	}
	return false
}

// wdaStartTimeout bounds starting the runner: the first start after the
// runner was installed takes the longest (10 to 20 s is common).
const wdaStartTimeout = 60 * time.Second

// OpenWDA opens a phone through its WebDriverAgent helper, starting the
// helper's test when it is not running yet.
func OpenWDA(ctx context.Context, info driver.Info) (*WDADevice, error) {
	s, err := startWDAFor(ctx, info)
	if err != nil {
		return nil, err
	}
	d, err := s.open(ctx, info)
	if err != nil {
		s.stop()
		return nil, err
	}
	return d, nil
}

// errAutomation is the phone waiting for its passcode: now and then iOS
// asks for it on the phone before a UI test runs, so before the helper can
// control the phone (see runnerLog). A passcode entered after the runner
// gave up still counts for the next start.
var errAutomation = errors.New("the iPhone asks on its screen for its passcode to allow automation")

// wdaStart is WebDriverAgent starting on a phone: the client that reaches
// it and, when Pinky started it, the runner's test run and the tunnel the
// run goes through.
type wdaStart struct {
	base goios.DeviceEntry
	c    *wdaClient
	run  *wdaRun
	l    *link
}

// startWDAFor starts the helper's test on a phone, unless WebDriverAgent
// already answers there (started by Xcode, or by an earlier Pinky).
func startWDAFor(ctx context.Context, info driver.Info) (*wdaStart, error) {
	base, err := findEntry(info.ID, info.Connection == "wifi")
	if err != nil {
		return nil, err
	}
	s := &wdaStart{base: base, c: newWDAUSB(base)}
	if s.ready(ctx) {
		return s, nil
	}
	bundleID, err := findWDARunner(base)
	if err != nil {
		return nil, err
	}
	if bundleID == "" {
		return nil, fmt.Errorf("the WebDriverAgent helper is not installed on %s: %w", info.ID, driver.ErrNotFound)
	}
	l, err := dialLinkEntry(ctx, base)
	if err != nil {
		return nil, err
	}
	bind := wdaBind(base, l.tunnel)
	if bind != "127.0.0.1" {
		s.c = newWDATunnel(l.tunnel)
	}
	s.run, s.l = startWDA(l.entry, bundleID, bind), l
	return s, nil
}

// ready reports whether WebDriverAgent answers and is ready.
func (s *wdaStart) ready(ctx context.Context) bool {
	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st, err := s.c.status(sctx)
	return err == nil && st.Ready
}

// running reports whether the start can still succeed: WebDriverAgent was
// running already, or the runner's test is still going.
func (s *wdaStart) running() bool {
	if s.run == nil {
		return true
	}
	alive, _ := s.run.alive()
	return alive
}

// open waits for WebDriverAgent to answer and opens the phone through it;
// the device then owns the run and the tunnel. It stops waiting when the
// run ends, and returns errAutomation as soon as the phone asks for its
// passcode, leaving the start to the caller, to stop or keep.
func (s *wdaStart) open(ctx context.Context, info driver.Info) (*WDADevice, error) {
	if s.run == nil {
		return openWDA(ctx, info, s.c, s.base, nil)
	}
	wctx, cancel := context.WithTimeout(ctx, wdaStartTimeout)
	defer cancel()
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-s.run.done:
				cancel()
				return
			case <-wctx.Done():
				return
			case <-t.C:
				if s.run.asking() {
					cancel()
					return
				}
			}
		}
	}()
	d, err := openWDA(wctx, info, s.c, s.base, s.run)
	if err != nil {
		if alive, rerr := s.run.alive(); !alive && rerr != nil {
			err = fmt.Errorf("start WebDriverAgent: %w", rerr)
		} else if alive && s.run.asking() {
			err = errAutomation
		}
		return nil, err
	}
	d.l = s.l
	return d, nil
}

// stop ends what the start began.
func (s *wdaStart) stop() {
	if s.run != nil {
		s.run.stop()
	}
	if s.l != nil {
		s.l.Close()
	}
}

func OpenWDAURL(ctx context.Context, base, mjpeg string) (*WDADevice, error) {
	return openWDA(ctx, driver.Info{ID: base, Platform: driver.IOS, Name: "WebDriverAgent", Connection: "usb", State: driver.StateOnline}, newWDAURL(base, mjpeg), goios.DeviceEntry{}, nil)
}

// Info implements driver.Device.
func (d *WDADevice) Info() driver.Info { return d.info }

// Orientation on this path. XCTest works in the interface's coordinates
// whatever way the phone is turned: the tree's frames, the gestures, the
// screen's size and the screenshots all follow the orientation (checked on
// an iPad Simulator, iPadOS 26.5, in all four), so only the screen's width
// and height change places. The MJPEG picture is the panel as it is, with
// the orientation in its EXIF tag, and is turned upright (frame.turn).
// Upside down, the tree and the taps stay right, but the screenshots and
// the picture come as the panel is with their tag saying up; GET /rotation
// (the app's interface orientation, unlike GET /orientation, which folds
// upside down into portrait) tells, and they are turned (pictureTurn).

const (
	wdaPortrait  = 1
	wdaLandscape = 2
)

// source reads WDA's tree and records the orientation from it. In
// landscape, XCTest gives an alert's window the panel's native frame
// instead of the interface's (seen on the iPad Simulator, iPadOS 26.5:
// Settings' Enable Dictation? alert, its buttons tall and narrow); such
// windows are turned upright the way the picture is.
func (d *WDADevice) source(ctx context.Context) (*wdaNode, error) {
	root, err := d.c.source(ctx)
	if err != nil {
		return nil, err
	}
	d.seeOrientation(root.Rect.Width, root.Rect.Height)
	if sideways := sidewaysWindows(root); len(sideways) > 0 {
		q := d.landscapeTurn(ctx)
		for _, w := range sideways {
			turnTree(w, root.Rect.Height, root.Rect.Width, q)
		}
	}
	return root, nil
}

// sidewaysWindows returns the windows of a landscape tree whose frame is
// the screen turned sideways: their frames are the panel's.
func sidewaysWindows(root *wdaNode) []*wdaNode {
	r := root.Rect
	if r.Width <= r.Height {
		return nil
	}
	var out []*wdaNode
	for i := range root.Children {
		w := &root.Children[i]
		if w.Type == "Window" && math.Abs(w.Rect.Width-r.Height) < 1 && math.Abs(w.Rect.Height-r.Width) < 1 {
			out = append(out, w)
		}
	}
	return out
}

// turnTree turns the frames of n and everything below it from a w×h frame
// q clockwise.
func turnTree(n *wdaNode, w, h float64, q quarter) {
	r := n.Rect
	switch q & 3 {
	case 1:
		n.Rect = wdaRect{X: h - r.Y - r.Height, Y: r.X, Width: r.Height, Height: r.Width}
	case 2:
		n.Rect = wdaRect{X: w - r.X - r.Width, Y: h - r.Y - r.Height, Width: r.Width, Height: r.Height}
	case 3:
		n.Rect = wdaRect{X: r.Y, Y: w - r.X - r.Width, Width: r.Height, Height: r.Width}
	}
	for i := range n.Children {
		turnTree(&n.Children[i], w, h, q)
	}
}

// landscapeTurn is how far the panel is turned in landscape: from GET
// /rotation (the interface's turn: 90 in LANDSCAPERIGHT, 270 in LANDSCAPE,
// the same as the MJPEG frames' EXIF), else from the picture's last frame
// when a stream runs, else from one frame of the MJPEG server. A quarter
// turn clockwise when none answers.
func (d *WDADevice) landscapeTurn(ctx context.Context) quarter {
	if q := d.interfaceTurn(ctx); q.swaps() {
		return q
	}
	d.videoMu.Lock()
	s, _ := d.video.(*frameStream)
	d.videoMu.Unlock()
	if s != nil {
		select {
		case <-s.done:
		default:
			if q, ok := s.lastTurn(); ok && q.swaps() {
				return q
			}
		}
	}
	if d.c.mjpeg != "" {
		fctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if src, err := openMJPEG(fctx, d.c.long, d.c.mjpeg); err == nil {
			f, err := src.next(fctx)
			src.Close()
			if err == nil && f.turn.swaps() {
				return f.turn
			}
		}
	}
	return 1
}

// seeOrientation records the interface's orientation from a size the right
// way up.
func (d *WDADevice) seeOrientation(w, h float64) {
	switch {
	case w <= 0 || h <= 0:
	case w > h:
		d.landscape.Store(wdaLandscape)
	default:
		d.landscape.Store(wdaPortrait)
	}
}

// size returns the screen's size in pixels the right way up, for the
// orientation last seen.
func (d *WDADevice) size() (int, int) {
	switch d.landscape.Load() {
	case wdaLandscape:
		return max(d.width, d.height), min(d.width, d.height)
	case wdaPortrait:
		return min(d.width, d.height), max(d.width, d.height)
	}
	return d.width, d.height
}

// sizeNow reads the orientation and returns the screen's size, for input
// that no read of the screen came before (the unlock swipe).
func (d *WDADevice) sizeNow(ctx context.Context) (int, int) {
	var ws struct {
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	}
	if err := d.c.do(ctx, d.c.hc, http.MethodGet, "/window/size", nil, &ws); err == nil {
		d.seeOrientation(ws.Width, ws.Height)
	}
	return d.size()
}

// toPoints converts screenshot pixels to WDA's points.
func (d *WDADevice) toPoints(p screen.Point) wdaPoint {
	w, h := d.size()
	x := math.Max(0, math.Min(float64(p.X), float64(w-1)))
	y := math.Max(0, math.Min(float64(p.Y), float64(h-1)))
	return wdaPoint{X: (x + 0.5) / d.scale, Y: (y + 0.5) / d.scale}
}

// Observe implements driver.Device: WDA's page source, with frames.
func (d *WDADevice) Observe(ctx context.Context, opt driver.ObserveOptions) (*screen.Screen, error) {
	var (
		wg       sync.WaitGroup
		root     *wdaNode
		srcErr   error
		app      wdaApp
		locked   bool
		lines    []ocrLine
		ocrDone  bool
		pictured sync.WaitGroup
		gate     *captureGate
		tm       wdaTimes
	)
	start := time.Now()
	known := make(chan struct{}) // the app and the lock state
	var knowing sync.WaitGroup
	knowing.Add(2)
	go func() { knowing.Wait(); close(known) }()
	wg.Add(3)
	go func() { defer wg.Done(); root, srcErr = d.source(ctx); tm.source = time.Since(start) }()
	go func() { defer wg.Done(); defer knowing.Done(); app, _ = d.c.activeApp(ctx) }()
	go func() {
		defer wg.Done()
		defer knowing.Done()
		locked, _ = d.lockState(ctx)
		tm.lock = time.Since(start)
	}()
	if opt.OCR {
		// The picture waits until the read may take it (capturegate.go):
		// WDA names the app in front long before the page source comes.
		gate = newCaptureGate(opt.MayCapture, func() {
			pictured.Add(1)
			go func() {
				defer pictured.Done()
				png, err := d.c.screenshot(ctx)
				tm.shot = time.Since(start)
				if err != nil {
					return
				}
				png = d.uprightShot(ctx, png)
				if l, err := recognizeText(png, ocrOptions{Languages: ocrLanguages}); err == nil {
					lines, ocrDone = l, true
				}
				tm.ocr = time.Since(start) - tm.shot
			}()
		})
		if opt.MayCapture != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-known
				if app.BundleID != "" {
					w, h := d.size()
					gate.early([]*screen.Screen{{Width: w, Height: h, Orientation: orientationOf(w, h),
						App: d.appInfo(ctx, app, nil), Locked: locked}})
				}
			}()
		}
	}
	wg.Wait()
	// At the end, once the picture is in (pictured.Wait below).
	defer func() { d.noteSlowRead(start, tm, root, opt.OCR) }()
	w, h := d.size() // d.source saw the orientation
	s := &screen.Screen{
		Width: w, Height: h,
		Orientation: "portrait",
		Locked:      locked,
		CapturedAt:  time.Now(),
	}
	s.Orientation = orientationOf(w, h)
	if srcErr == nil {
		s.App = d.appInfo(ctx, app, root)
	}
	if gate != nil {
		use := gate.late(s)
		pictured.Wait()
		if !use {
			lines, ocrDone = nil, false
		}
	}
	if srcErr != nil {
		return nil, fmt.Errorf("observe: %w", srcErr)
	}
	scale := d.scale
	if root.Rect.Width > 0 {
		scale = float64(w) / root.Rect.Width
	}
	s.Elements, s.Keyboard = wdaScreenElements(root, scale, w, h, s.App.ID)
	if len(s.Elements) == 0 && !locked {
		// Nothing of the app in its tree: the lock screen may be in front
		// while lockdown says unlocked (GitHub issue #7: a first read gave
		// WeChat with only the lock screen's charging line from OCR). WDA
		// knows whether the lock screen shows.
		if l, err := d.c.locked(ctx); err == nil && l {
			s.Locked = true
			return s, nil
		}
	}
	if ocrDone && len(lines) > 0 {
		if top, kb := keyboardFromOCR(lines); kb {
			s.Keyboard = true
			kept := lines[:0:0]
			for _, l := range lines {
				if l.Y < top {
					kept = append(kept, l)
				}
			}
			lines = kept
		}
		s.Elements = append(s.Elements, ocrExtras(s.Elements, lines, w, h)...)
	}
	return s, nil
}

// wdaTimes are when the parts of a read ended, from its start; ocr is the
// recognition alone.
type wdaTimes struct{ source, lock, shot, ocr time.Duration }

// slowRead is how long a read may take before the log says what took the
// time.
const slowRead = 3 * time.Second

// noteSlowRead logs a read that took slowRead or more, with its parts and
// the size of the tree (a busy app's tree takes WDA seconds).
func (d *WDADevice) noteSlowRead(start time.Time, tm wdaTimes, root *wdaNode, ocr bool) {
	total := time.Since(start)
	if d.log == nil || total < slowRead {
		return
	}
	nodes := 0
	var count func(n *wdaNode)
	count = func(n *wdaNode) {
		nodes++
		for i := range n.Children {
			count(&n.Children[i])
		}
	}
	if root != nil {
		count(root)
	}
	args := []any{"device", d.info.ID, "total", total.Round(time.Millisecond), "source", tm.source.Round(time.Millisecond),
		"nodes", nodes, "lock", tm.lock.Round(time.Millisecond), "connection", d.info.Connection}
	if ocr {
		args = append(args, "screenshot", tm.shot.Round(time.Millisecond), "ocr", tm.ocr.Round(time.Millisecond))
	}
	d.log.Info("iphone: slow screen read through WebDriverAgent", args...)
}

// appInfo names the app in front: WDA's active app, with the name from the
// app list (WDA leaves SpringBoard's empty).
func (d *WDADevice) appInfo(ctx context.Context, a wdaApp, root *wdaNode) screen.App {
	if a.BundleID == springBoard {
		return screen.App{Name: "Home Screen", ID: springBoard}
	}
	out := screen.App{ID: a.BundleID, Name: a.Name}
	if out.Name == "" && root != nil {
		out.Name = strings.TrimSpace(strOr(root.Label))
	}
	if out.Name == "" && a.BundleID != "" {
		if apps, err := d.Apps(ctx); err == nil {
			for _, x := range apps {
				if x.ID == a.BundleID {
					out.Name = x.Name
				}
			}
		}
	}
	return out
}

// Screenshot implements driver.Device.
func (d *WDADevice) Screenshot(ctx context.Context) (image.Image, error) {
	b, err := d.c.screenshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("screenshot: decode: %w", err)
	}
	return turnImage(img, d.pictureTurn(ctx)), nil
}

// Tap implements driver.Device.
func (d *WDADevice) Tap(ctx context.Context, p screen.Point, opt driver.TapOptions) error {
	hold := 60 * time.Millisecond
	if opt.LongPress {
		hold = 800 * time.Millisecond
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	return d.c.gesture(ctx, []wdaPoint{d.toPoints(p)}, 0, hold)
}

// Swipe implements driver.Device.
func (d *WDADevice) Swipe(ctx context.Context, from, to screen.Point, dur time.Duration) error {
	if dur <= 0 {
		dur = 300 * time.Millisecond
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	return d.c.gesture(ctx, []wdaPoint{d.toPoints(from), d.toPoints(to)}, dur, 0)
}

// Type implements driver.Device. XCTest types any text, CJK and emoji
// included, into the field with the keyboard focus.
func (d *WDADevice) Type(ctx context.Context, text string, opt driver.TypeOptions) error {
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	if !opt.Append {
		if err := d.c.clearFocused(ctx); err != nil {
			if errors.Is(err, errNoFocus) {
				return fmt.Errorf("type: %v: %w", err, driver.ErrNotFound)
			}
			return fmt.Errorf("type: %w", err)
		}
	}
	if text != "" {
		if err := d.c.keys(ctx, text); err != nil {
			return fmt.Errorf("type: %w", err)
		}
	}
	if opt.Submit {
		if err := d.c.keys(ctx, "\n"); err != nil {
			return fmt.Errorf("type: submit: %w", err)
		}
	}
	return nil
}

// wdaKeys are the keys WDA types as characters (XCUIKeyboardKey values).
var wdaKeys = map[driver.Key]string{
	driver.KeyEnter:         "\n",
	driver.KeyTab:           "\t",
	driver.KeyDelete:        "\b",
	driver.KeyForwardDelete: "\u007f",
	driver.KeyEscape:        "\u001b",
}

// Key implements driver.Device. Home is XCTest's Home button; Back is a
// swipe from the left edge, as on the other path.
func (d *WDADevice) Key(ctx context.Context, k driver.Key) error {
	switch k {
	case driver.KeyHome:
		d.inputMu.Lock()
		defer d.inputMu.Unlock()
		return d.c.home(ctx)
	case driver.KeyBack:
		return d.back(ctx)
	case driver.KeyRecents:
		return fmt.Errorf("recents on iPhone: %w", driver.ErrUnsupported)
	}
	s, ok := wdaKeys[k]
	if !ok {
		return fmt.Errorf("key %q: %w", k, driver.ErrUnsupported)
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	return d.c.keys(ctx, s)
}

// back taps the navigation bar's back button when the screen has one, and
// otherwise swipes from the left edge. XCTest's synthesized touches do not
// always start the system's edge swipe (not on the iOS 18 Simulator), so
// the button comes first.
func (d *WDADevice) back(ctx context.Context) error {
	if root, err := d.source(ctx); err == nil {
		if p, ok := backButton(root, d.scale); ok {
			return d.Tap(ctx, p, driver.TapOptions{})
		}
	}
	w, h := d.size()
	y := h / 2
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	from, to := d.toPoints(screen.Point{X: 0, Y: y}), d.toPoints(screen.Point{X: w * 7 / 10, Y: y})
	from.X = 0
	return d.c.gesturePath(ctx, []wdaPoint{from, from, to}, []time.Duration{0, 50 * time.Millisecond, 250 * time.Millisecond}, 0)
}

// backButton finds a navigation bar's back button. UIKit names it
// BackButton (its label is the page it goes back to); an iPad's split view
// has a bar for the sidebar and one for the page beside it, and the back
// button of the widest, the page's, wins. Without one, the first button at
// the leading edge of a bar, outside its search field: before, the first
// bar's first button in the screen's left third, which on an iPad's
// Settings was the sidebar search field's Dictate button (iPad Simulator,
// iPadOS 26.5).
func backButton(root *wdaNode, scale float64) (screen.Point, bool) {
	var bars []*wdaNode
	var find func(n *wdaNode, depth int)
	find = func(n *wdaNode, depth int) {
		if depth > 60 {
			return
		}
		if n.Type == "NavigationBar" && bool(n.Visible) {
			bars = append(bars, n)
			return
		}
		for i := range n.Children {
			find(&n.Children[i], depth+1)
		}
	}
	find(root, 0)
	var btn *wdaNode
	var barW float64
	for _, bar := range bars {
		if b := namedBack(bar, 0); b != nil && bar.Rect.Width >= barW {
			btn, barW = b, bar.Rect.Width
		}
	}
	for i := 0; btn == nil && i < len(bars); i++ {
		btn = leadingButton(bars[i], bars[i].Rect.X+bars[i].Rect.Width/3, 0)
	}
	if btn == nil {
		return screen.Point{}, false
	}
	return screen.Point{
		X: int(math.Round((btn.Rect.X + btn.Rect.Width/2) * scale)),
		Y: int(math.Round((btn.Rect.Y + btn.Rect.Height/2) * scale)),
	}, true
}

// namedBack finds the button UIKit names BackButton below n.
func namedBack(n *wdaNode, depth int) *wdaNode {
	if depth > 10 {
		return nil
	}
	if n.Type == "Button" && bool(n.Visible) && n.Rect.Width > 0 &&
		(strOr(n.Identifier) == "BackButton" || strOr(n.Name) == "BackButton") {
		return n
	}
	for i := range n.Children {
		if b := namedBack(&n.Children[i], depth+1); b != nil {
			return b
		}
	}
	return nil
}

// leadingButton finds the first button below n that starts before limit,
// leaving out search fields and what is in them.
func leadingButton(n *wdaNode, limit float64, depth int) *wdaNode {
	if depth > 10 || n.Type == "SearchField" {
		return nil
	}
	if n.Type == "Button" && bool(n.Visible) && n.Rect.Width > 0 && n.Rect.X < limit {
		return n
	}
	for i := range n.Children {
		if b := leadingButton(&n.Children[i], limit, depth+1); b != nil {
			return b
		}
	}
	return nil
}

// Launch implements driver.Device. A running app comes to the front without
// restarting.
func (d *WDADevice) Launch(ctx context.Context, app string) error {
	id, err := d.resolveApp(ctx, app)
	if err != nil {
		return err
	}
	if err := d.c.activate(ctx, id); err != nil {
		return fmt.Errorf("launch %s: %w", id, err)
	}
	d.logs.launched(id, 0)
	return nil
}

// resolveApp turns a display name into a bundle id through the app list.
// Without one (WDA at a URL), the name is taken as a bundle id.
func (d *WDADevice) resolveApp(ctx context.Context, app string) (string, error) {
	app = strings.TrimSpace(app)
	if app == "" {
		return "", fmt.Errorf("no app given: %w", driver.ErrNotFound)
	}
	if d.entry.DeviceID == 0 && d.appsFn == nil {
		return app, nil
	}
	apps, err := d.Apps(ctx)
	if err != nil {
		return "", err
	}
	for _, a := range apps {
		if a.ID == app {
			return a.ID, nil
		}
	}
	for _, a := range apps {
		if strings.EqualFold(a.Name, app) || strings.EqualFold(a.ID, app) {
			return a.ID, nil
		}
	}
	return "", fmt.Errorf("app %q: %w", app, driver.ErrNotFound)
}

// Apps implements driver.Device, through the installation proxy, or simctl
// on a Simulator.
func (d *WDADevice) Apps(ctx context.Context) ([]driver.App, error) {
	if d.appsFn != nil {
		return d.appsFn(ctx)
	}
	if d.entry.DeviceID == 0 {
		return nil, fmt.Errorf("app list: %w", driver.ErrUnsupported)
	}
	d.appMu.Lock()
	if d.apps == nil {
		d.apps = &appCatalog{}
	}
	c := d.apps
	d.appMu.Unlock()
	c.mu.Lock()
	fresh := c.apps != nil && time.Since(c.at) < appCacheTTL
	apps := append([]driver.App(nil), c.apps...)
	c.mu.Unlock()
	if fresh {
		return apps, nil
	}
	type result struct {
		list appList
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		list, err := browseApps(&link{base: d.entry})
		ch <- result{list, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		c.set(r.list)
		return append([]driver.App(nil), r.list.apps...), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Locked implements driver.LockReporter: whether the phone needs its
// passcode, from lockdown as on the CoreDevice path.
func (d *WDADevice) Locked(ctx context.Context) (bool, error) {
	// The gateway asks this every few seconds of every open phone, and
	// lockdown answers it with WDA gone: say so here, so a phone whose
	// runner ended drops before an agent's next call fails.
	if d.ended() {
		return false, fmt.Errorf("the WebDriverAgent runner ended: %w", driver.ErrOffline)
	}
	return d.lockState(ctx)
}

// Stream implements driver.Device: WDA's MJPEG picture, encoded to H.264.
// The picture is the screen at half size (wdaSettings).
func (d *WDADevice) Stream(ctx context.Context, opt driver.StreamOptions) (driver.Stream, error) {
	if _, err := d.c.session(ctx); err != nil { // applies the MJPEG settings
		return nil, err
	}
	d.interfaceTurn(ctx) // the first frames' turn
	s, err := startFrameStreamLog(ctx, d.log, func(ctx context.Context) (frameSource, error) {
		src, err := openMJPEG(ctx, d.c.long, d.c.mjpeg)
		if r, ok := src.(*mjpegReader); ok {
			// Upside down, the frames' EXIF says up (see pictureTurn).
			r.upside = d.cachedPictureTurn
		}
		return src, err
	}, d.width, d.height)
	if err != nil {
		return nil, err
	}
	if fs, ok := s.(*frameStream); ok {
		go d.followRotation(fs.done)
	}
	d.videoMu.Lock()
	d.video = s
	d.videoMu.Unlock()
	return s, nil
}

// rotationFresh is how long a read of GET /rotation is trusted.
const rotationFresh = time.Second

// interfaceTurn is how far the interface is turned clockwise from portrait,
// from GET /rotation, read again when the last read is older than
// rotationFresh. 0 when WDA does not say.
func (d *WDADevice) interfaceTurn(ctx context.Context) quarter {
	if at := d.uiAt.Load(); at != 0 && time.Since(time.Unix(0, at)) < rotationFresh {
		return quarter(d.ui.Load()-1) & 3
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	z, err := d.c.rotation(rctx)
	cancel()
	if err != nil {
		if n := d.ui.Load(); n > 0 {
			return quarter(n-1) & 3
		}
		return 0
	}
	q := quarter((z/90)%4+4) & 3
	d.ui.Store(int32(q) + 1)
	d.uiAt.Store(time.Now().UnixNano())
	return q
}

// pictureTurn is the turn XCTest's pictures need beyond their own EXIF tag:
// upside down, XCTest's screenshots and MJPEG frames are the panel as it is
// and their tag says up, though the tree and the gestures are the
// interface's (iPad Simulator, iPadOS 26.5). In landscape the tag is right
// and WDA's screenshots come upright already.
func (d *WDADevice) pictureTurn(ctx context.Context) quarter {
	if d.interfaceTurn(ctx) == 2 {
		return 2
	}
	return 0
}

// cachedPictureTurn is pictureTurn from the last read, for the MJPEG
// frames, which followRotation keeps fresh.
func (d *WDADevice) cachedPictureTurn() quarter {
	if d.ui.Load() == 3 { // two quarter turns, plus one
		return 2
	}
	return 0
}

// followRotation reads GET /rotation every second until done is closed, so
// the picture turns with the device.
func (d *WDADevice) followRotation(done <-chan struct{}) {
	t := time.NewTicker(rotationFresh)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			d.interfaceTurn(context.Background())
		case <-done:
			return
		}
	}
}

// uprightShot turns a screenshot upright for OCR when XCTest left it
// upside down (pictureTurn), as a JPEG, which Vision reads as well.
func (d *WDADevice) uprightShot(ctx context.Context, b []byte) []byte {
	q := d.pictureTurn(ctx)
	if q == 0 {
		return b
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return b
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, turnImage(img, q), &jpeg.Options{Quality: 95}); err != nil {
		return b
	}
	return buf.Bytes()
}

// videoSize is the size of the last stream's picture now, or 0, 0: it
// changes when the phone turns.
func (d *WDADevice) videoSize() (int, int) {
	d.videoMu.Lock()
	s := d.video
	d.videoMu.Unlock()
	if s == nil {
		return 0, 0
	}
	return s.Size()
}

// Close implements driver.Device. It ends the WebDriver session and stops
// the test run Pinky started; WDA itself stays installed.
func (d *WDADevice) Close() error {
	d.closeOnce.Do(func() {
		d.StopAppLogs()
		d.live.close() // the player for the mirror's drag ends here
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		d.c.deleteSession(ctx)
		cancel()
		if d.run != nil {
			d.run.stop()
		}
		if d.l != nil {
			d.l.Close()
		}
	})
	return nil
}

var (
	_ driver.Device       = (*WDADevice)(nil)
	_ driver.LockReporter = (*WDADevice)(nil)
	_ driver.Settler      = (*WDADevice)(nil)
)
