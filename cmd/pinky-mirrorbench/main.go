// Command pinky-mirrorbench measures how smoothly a phone's live picture
// reaches the mirror. It opens the phone through Pinky's own driver, as the
// app does, decodes its stream as the mirror does, and drags a list with
// raw touches while it compares each picture with the one before:
//
//   - the time from opening the phone, and from asking for the stream, to
//     the first picture
//   - the pictures that changed per second while the list moved, and the
//     gaps between them
//   - the time from the first move of the finger to the first picture that
//     changed
//   - how many of those pictures a window painting 60 or 120 times a second
//     would never show
//
// Run it under scripts/devlock.sh, as everything that touches a phone:
//
//	scripts/devlock.sh emulator-5560 go run ./cmd/pinky-mirrorbench -android emulator-5560
//	scripts/devlock.sh emulator-5560 go run ./cmd/pinky-mirrorbench -android emulator-5560 -bg
//	scripts/devlock.sh <udid> go run ./cmd/pinky-mirrorbench -ios <udid>
//	go run ./cmd/pinky-mirrorbench -wda http://127.0.0.1:18200 -mjpeg http://127.0.0.1:19200
//
// It drags in Settings, which it opens first (-app picks another app), and
// leaves the list where it found it, give or take. On an iPhone it drags
// with WebDriverAgent's swipe, because the helper plays its raw touches
// back when the finger lifts; -raw-touch measures the raw touches
// instead, as the mirror sends them. The numbers are in
// docs/notes/mirror-smoothness.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/DIYgod/pinky/internal/driver"
	"github.com/DIYgod/pinky/internal/driver/android"
	"github.com/DIYgod/pinky/internal/driver/android/adb"
	"github.com/DIYgod/pinky/internal/driver/ios"
	"github.com/DIYgod/pinky/internal/video"
)

type config struct {
	android, ios, wda, mjpeg string
	bg                       bool
	app                      string
	trials, warm             int
	drag                     time.Duration
	still, settle            time.Duration
	// scrcpy's settings, for trying others (Android only).
	maxFPS, bitRate, maxSize int
	codec                    string
	rawTouch                 bool
	verbose, json            bool
}

func main() {
	var c config
	flag.StringVar(&c.android, "android", "", "the Android phone or emulator, by adb serial")
	flag.StringVar(&c.ios, "ios", "", "the iPhone, by UDID")
	flag.StringVar(&c.wda, "wda", "", "a WebDriverAgent server's URL, such as a Simulator's")
	flag.StringVar(&c.mjpeg, "mjpeg", "", "the WebDriverAgent's MJPEG server's URL")
	flag.BoolVar(&c.bg, "bg", false, "Android: measure a background display with the app on it")
	flag.StringVar(&c.app, "app", "", "the app to drag in (default Settings)")
	flag.IntVar(&c.trials, "trials", 6, "drags to make")
	flag.IntVar(&c.warm, "warm", 3, "times to reopen the stream for the first picture")
	flag.DurationVar(&c.drag, "drag", time.Second, "how long each drag moves")
	flag.DurationVar(&c.still, "still", 3*time.Second, "how long to watch a still screen")
	flag.DurationVar(&c.settle, "settle", 8*time.Second, "how long to watch the screen after the last drag")
	flag.IntVar(&c.maxFPS, "max-fps", 0, "Android: scrcpy's max_fps (0: the driver's default)")
	flag.IntVar(&c.bitRate, "bitrate", 0, "Android: scrcpy's video bit rate in bits per second (0: the driver's default)")
	flag.IntVar(&c.maxSize, "max-size", 0, "Android: scrcpy's max_size (0: native)")
	flag.StringVar(&c.codec, "codec", "", "Android: h264 or h265 (empty: the driver's default)")
	flag.BoolVar(&c.rawTouch, "raw-touch", false, "iOS: drag with raw touches, as the mirror sends them, instead of WebDriverAgent's swipe")
	flag.BoolVar(&c.verbose, "v", false, "print every drag")
	flag.BoolVar(&c.json, "json", false, "print the summary as JSON")
	flag.Parse()
	n := 0
	for _, s := range []string{c.android, c.ios, c.wda} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		fmt.Fprintln(os.Stderr, "pinky-mirrorbench: give one of -android, -ios or -wda")
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, c); err != nil {
		fmt.Fprintln(os.Stderr, "pinky-mirrorbench:", err)
		os.Exit(1)
	}
}

// summary is what a run found, medians over the drags.
type summary struct {
	Device     string  `json:"device"`
	Codec      string  `json:"codec"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Decoder    string  `json:"decoder"`
	OpenMS     float64 `json:"open_ms"`
	ColdMS     float64 `json:"cold_first_picture_ms"`
	ColdPktMS  float64 `json:"cold_first_packet_ms"`
	WarmMS     float64 `json:"warm_first_picture_ms"`
	StillFPS   float64 `json:"still_pictures_per_s"`
	StillMoved float64 `json:"still_changed_per_s"`
	StillReps  float64 `json:"still_repeats_per_s"`
	StillKbps  float64 `json:"still_kbit_per_s"`
	StillPaint float64 `json:"still_paint_share"`
	StillAll   float64 `json:"still_paint_share_with_repeats"`
	Trials     int     `json:"trials"`
	TouchRoute string  `json:"touch_route"`
	LatencyMS  float64 `json:"latency_ms"`
	ReadyMS    float64 `json:"latency_decoded_ms"`
	FPS        float64 `json:"moving_fps"`
	SentFPS    float64 `json:"sent_fps"`
	GapMS      float64 `json:"gap_median_ms"`
	GapP95MS   float64 `json:"gap_p95_ms"`
	GapMaxMS   float64 `json:"gap_max_ms"`
	Mbps       float64 `json:"mbit_per_s"`
	Drop60     float64 `json:"paint60_drop"`
	Drop120    float64 `json:"paint120_drop"`
	AppFPS     float64 `json:"app_fps,omitempty"`
	SettleS    float64 `json:"after_drag_pictures_s"`
	SettleReps int     `json:"after_drag_repeats"`
	SettlePics int     `json:"after_drag_pictures"`
	SettlePnt  float64 `json:"after_drag_paint_s"`
	SettleAll  float64 `json:"after_drag_paint_s_with_repeats"`
	DecodeMS   float64 `json:"decode_ms"`
	DecodeMax  float64 `json:"decode_max_ms"`
	Undecoded  int     `json:"undecoded"`
	DecodeErrs int     `json:"decode_errors"`
}

func run(ctx context.Context, c config) error {
	if err := video.Load(); err != nil {
		return err
	}
	var sum summary
	start := time.Now()
	dev, gfx, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer dev.Close()
	sum.OpenMS = ms(time.Since(start))
	sum.Device = describe(dev.Info())
	// Which way the drags move the list, so every number below says where
	// it came from.
	route := touchRoute(dev, c.rawTouch)
	sum.TouchRoute = route
	logf := func(format string, args ...any) {
		if !c.json {
			fmt.Printf(format+"\n", args...)
		}
	}
	logf("%s, opened in %.0f ms", sum.Device, sum.OpenMS)

	// The first picture: once cold, right after opening, then warm, as
	// when the mirror opens again on a phone Pinky has open.
	var rec *recorder
	for i := 0; i <= c.warm; i++ {
		if rec != nil {
			rec.stop()
		}
		ff, r, err := firstPicture(ctx, dev)
		if err != nil {
			return fmt.Errorf("first picture: %w", err)
		}
		rec = r
		if i == 0 {
			sum.ColdMS, sum.ColdPktMS = ms(ff.picture), ms(ff.packet)
			logf("cold: Stream %.0f ms, first packet %.0f ms, first picture %.0f ms", ms(ff.call), ms(ff.packet), ms(ff.picture))
		} else {
			sum.WarmMS += ms(ff.picture) / float64(c.warm)
			if c.verbose {
				logf("warm: Stream %.0f ms, first packet %.0f ms, first picture %.0f ms", ms(ff.call), ms(ff.packet), ms(ff.picture))
			}
		}
	}
	defer rec.stop()
	if c.warm > 0 {
		logf("warm: first picture %.0f ms (mean of %d)", sum.WarmMS, c.warm)
	}
	sum.Codec = string(rec.codec)
	sum.Width, sum.Height = rec.size()
	sum.Decoder = rec.hardware()
	logf("stream: %s %dx%d, decoder %s", sum.Codec, sum.Width, sum.Height, sum.Decoder)

	// What a still screen sends.
	if c.still > 0 {
		rec.waitStill(ctx, 700*time.Millisecond, 5*time.Second)
		from := time.Now()
		if err := sleep(ctx, c.still); err != nil {
			return err
		}
		pics := rec.since(from)
		bytes, changed, moved, repeats := 0, 0, 0, 0
		top, bottom := 1.0, 0.0
		var peaks []int
		for _, p := range pics {
			bytes += p.bytes
			if p.changed {
				changed++
			}
			if p.repeat {
				repeats++
			} else if !p.changed {
				peaks = append(peaks, p.peak)
			}
			if p.moved > 0 {
				moved++
				top, bottom = min(top, p.top), max(bottom, p.bottom)
			}
		}
		sum.StillFPS = float64(len(pics)) / c.still.Seconds()
		sum.StillMoved = float64(changed) / c.still.Seconds()
		sum.StillReps = float64(repeats) / c.still.Seconds()
		sum.StillKbps = float64(bytes*8) / c.still.Seconds() / 1000
		to := from.Add(c.still)
		sum.StillPaint = painting(pics, from, to, true).Seconds() / c.still.Seconds()
		sum.StillAll = painting(pics, from, to, false).Seconds() / c.still.Seconds()
		logf("still screen: %.1f pictures/s (%.1f changed, %.1f repeats), %.0f kbit/s; the mirror paints %.0f%% of the time, %.0f%% if it woke for repeats",
			sum.StillFPS, sum.StillMoved, sum.StillReps, sum.StillKbps, sum.StillPaint*100, sum.StillAll*100)
		if c.verbose && len(peaks) > 0 {
			slices.Sort(peaks)
			logf("  %d were neither repeats nor changed: a sample moved by %d levels at most (median %d)", len(peaks), peaks[len(peaks)-1], median(peaks))
		}
		if c.verbose {
			timeline(logf, "still screen", pics, from, c.still)
		}
		if c.verbose && moved > 0 {
			logf("  %d of them moved a few samples, from %.0f%% to %.0f%% of the height", moved, top*100, bottom*100)
		}
	}

	// The drags.
	var results []trialResult
	var lastMove time.Time
	for i := 0; i < c.trials; i++ {
		if !rec.waitStill(ctx, 700*time.Millisecond, 5*time.Second) {
			logf("drag %d: the screen did not stop changing; dragging anyway", i+1)
		}
		up := i%2 == 0 // the finger moves up first, scrolling down the list
		t, err := drag(ctx, dev, rec, gfx, up, c.drag, c.rawTouch)
		if err != nil {
			return fmt.Errorf("drag %d: %w", i+1, err)
		}
		lastMove = t.end
		r, ok := analyze(t)
		if !ok {
			logf("drag %d: no picture changed", i+1)
			continue
		}
		results = append(results, r)
		if c.verbose {
			app := ""
			if r.AppFPS > 0 {
				app = fmt.Sprintf(", app %.1f fps", r.AppFPS)
			}
			logf("drag %d: latency %.0f ms (decoded %.0f), %.1f fps moving (%.1f sent), gaps %.1f/%.1f/%.1f ms, %.1f Mbit/s, paint drops 60 Hz %.0f%% 120 Hz %.0f%%%s (%s)",
				i+1, ms(r.Latency), ms(r.Ready), r.FPS, r.Sent, ms(r.GapMedian), ms(r.GapP95), ms(r.GapMax), r.Mbps, r.Drop60*100, r.Drop120*100, app, route)
		}
	}
	if len(results) == 0 {
		return errors.New("no drag moved the picture")
	}
	// How long the phone goes on sending after the screen stops, and how
	// long the mirror paints for it: with repeats skipped, as the app's
	// mirror does, and if it woke for them.
	if c.settle > 0 {
		end := lastMove.Add(c.settle)
		if err := sleep(ctx, time.Until(end)); err != nil {
			return err
		}
		pics := rec.between(lastMove, end)
		for _, p := range pics {
			sum.SettleS = p.at.Sub(lastMove).Seconds()
			if p.repeat {
				sum.SettleReps++
			}
		}
		sum.SettlePics = len(pics)
		sum.SettlePnt = painting(pics, lastMove, end, true).Seconds()
		sum.SettleAll = painting(pics, lastMove, end, false).Seconds()
		if c.verbose {
			timeline(logf, "after last drag", pics, lastMove, c.settle)
		}
	}
	pick := func(f func(trialResult) float64) float64 {
		v := make([]float64, len(results))
		for i, r := range results {
			v[i] = f(r)
		}
		return median(v)
	}
	sum.Trials = len(results)
	sum.LatencyMS = pick(func(r trialResult) float64 { return ms(r.Latency) })
	sum.ReadyMS = pick(func(r trialResult) float64 { return ms(r.Ready) })
	sum.FPS = pick(func(r trialResult) float64 { return r.FPS })
	sum.SentFPS = pick(func(r trialResult) float64 { return r.Sent })
	sum.GapMS = pick(func(r trialResult) float64 { return ms(r.GapMedian) })
	sum.GapP95MS = pick(func(r trialResult) float64 { return ms(r.GapP95) })
	sum.GapMaxMS = pick(func(r trialResult) float64 { return ms(r.GapMax) })
	sum.Mbps = pick(func(r trialResult) float64 { return r.Mbps })
	sum.Drop60 = pick(func(r trialResult) float64 { return r.Drop60 })
	sum.Drop120 = pick(func(r trialResult) float64 { return r.Drop120 })
	sum.AppFPS = pick(func(r trialResult) float64 { return r.AppFPS })
	sum.DecodeMS, sum.DecodeMax, sum.Undecoded, sum.DecodeErrs = rec.decodeStats()
	if c.json {
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "  ")
		return e.Encode(sum)
	}
	fmt.Printf("\n%d drags, medians:\n", sum.Trials)
	fmt.Printf("  touch to picture   %.0f ms, decoded %.0f ms (%s)\n", sum.LatencyMS, sum.ReadyMS, sum.TouchRoute)
	fmt.Printf("  moving             %.1f changed pictures/s (%.1f sent)\n", sum.FPS, sum.SentFPS)
	fmt.Printf("  gaps               median %.1f ms, p95 %.1f ms, max %.1f ms\n", sum.GapMS, sum.GapP95MS, sum.GapMaxMS)
	fmt.Printf("  bit rate           %.1f Mbit/s\n", sum.Mbps)
	fmt.Printf("  painting drops     %.0f%% at 60 Hz, %.0f%% at 120 Hz\n", sum.Drop60*100, sum.Drop120*100)
	if sum.AppFPS > 0 {
		fmt.Printf("  the app drew       %.1f frames/s\n", sum.AppFPS)
	}
	if c.settle > 0 {
		more := ""
		if c.settle.Seconds()-sum.SettleS < 0.5 {
			more = ", and on when it stopped watching"
		}
		fmt.Printf("  after last drag    pictures for %.2f s%s, %d of %d repeats; the mirror paints %.2f s, %.2f s if it woke for repeats\n",
			sum.SettleS, more, sum.SettleReps, sum.SettlePics, sum.SettlePnt, sum.SettleAll)
	}
	fmt.Printf("  decode             %.1f ms median, %.1f ms max; %d packets without a picture, %d errors\n", sum.DecodeMS, sum.DecodeMax, sum.Undecoded, sum.DecodeErrs)
	return nil
}

// timeline logs, second by second from from, how many pictures came, how
// many were repeats, and how far the others moved a sample at most.
func timeline(logf func(string, ...any), what string, pics []pic, from time.Time, d time.Duration) {
	type second struct{ pics, reps, peak int }
	per := make([]second, max(int(d.Round(time.Second)/time.Second), 1))
	for _, p := range pics {
		s := &per[min(max(int(p.at.Sub(from)/time.Second), 0), len(per)-1)]
		s.pics++
		if p.repeat {
			s.reps++
		} else {
			s.peak = max(s.peak, p.peak)
		}
	}
	for i, s := range per {
		if s.pics > 0 {
			logf("  %s, %d to %d s: %d pictures, %d repeats; the others moved a sample by %d levels at most", what, i, i+1, s.pics, s.reps, s.peak)
		}
	}
}

// open opens the phone the flags name, starts the app to drag in, and
// returns, for Android, a way to count the frames the app draws.
func open(ctx context.Context, c config) (driver.Device, *gfxInfo, error) {
	switch {
	case c.android != "":
		cl := &adb.Client{}
		p := android.NewProvider(android.Config{
			Client:       cl,
			MaxFPS:       c.maxFPS,
			VideoBitRate: c.bitRate,
			MaxSize:      c.maxSize,
			VideoCodec:   c.codec,
		})
		d, err := p.OpenDevice(ctx, c.android)
		if err != nil {
			return nil, nil, err
		}
		app := c.app
		if app == "" {
			app = "com.android.settings"
		}
		gfx := &gfxInfo{client: cl, serial: d.Serial(), pkg: app}
		if c.bg {
			b, err := d.OpenBackground(ctx, driver.BackgroundOptions{App: app})
			if err != nil {
				d.Close()
				return nil, nil, fmt.Errorf("background display: %w", err)
			}
			if err := sleep(ctx, 2*time.Second); err != nil {
				return nil, nil, err
			}
			return closeBoth{b, d}, gfx, nil
		}
		if err := d.Launch(ctx, app); err != nil {
			d.Close()
			return nil, nil, fmt.Errorf("launch %s: %w", app, err)
		}
		return d, gfx, sleep(ctx, 1500*time.Millisecond)
	case c.ios != "":
		p := ios.NewProvider()
		// As Pinky does when the person uses the phone over Wi-Fi, so a
		// phone without a cable can be measured too.
		p.SetWiFi(true)
		p.UseOverWiFi(c.ios)
		if _, err := p.List(ctx); err != nil {
			return nil, nil, err
		}
		d, err := p.Open(ctx, c.ios)
		if err != nil {
			return nil, nil, err
		}
		return d, nil, launch(ctx, d, c.app, "com.apple.Preferences")
	default:
		d, err := ios.OpenWDAURL(ctx, c.wda, c.mjpeg)
		if err != nil {
			return nil, nil, err
		}
		return d, nil, launch(ctx, d, c.app, "com.apple.Preferences")
	}
}

func launch(ctx context.Context, d driver.Device, app, def string) error {
	if app == "" {
		app = def
	}
	if err := d.Launch(ctx, app); err != nil {
		d.Close()
		return fmt.Errorf("launch %s: %w", app, err)
	}
	return sleep(ctx, 1500*time.Millisecond)
}

// closeBoth is a background display that closes the phone with itself.
type closeBoth struct {
	driver.Device
	phone driver.Device
}

func (c closeBoth) Close() error {
	return errors.Join(c.Device.Close(), c.phone.Close())
}

// Touch passes the background display's raw touches through.
func (c closeBoth) Touch(ctx context.Context, a driver.TouchAction, p screenPoint) error {
	t, ok := c.Device.(driver.Toucher)
	if !ok {
		return driver.ErrUnsupported
	}
	return t.Touch(ctx, a, p)
}

func describe(i driver.Info) string {
	s := strings.TrimSpace(i.Name)
	if i.Model != "" && i.Model != s {
		s += " (" + i.Model + ")"
	}
	if i.OSVersion != "" {
		s += ", " + string(i.Platform) + " " + i.OSVersion
	}
	if i.Backend != "" {
		s += ", " + i.Backend
	}
	if i.Connection != "" {
		s += ", " + i.Connection
	}
	return s
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
