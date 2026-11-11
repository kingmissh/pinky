package ios

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DIYgod/pinky/internal/driver"
	"github.com/DIYgod/pinky/internal/screen"
	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/installationproxy"
)

// wdaSettingsTree is the shape WDA 16 sends for iOS 18's Settings, cut down:
// a navigation bar, a search field, rows (a Cell holding an accessible
// Button with the title), the status bar and a keyboard.
const wdaSettingsTree = `{
 "type": "Application", "label": "Settings", "name": "Settings", "isEnabled": "1", "isVisible": "1", "isAccessible": "0",
 "rect": {"x": 0, "y": 0, "width": 393, "height": 852},
 "children": [
  {"type": "StatusBar", "isVisible": "1", "rect": {"x": 0, "y": 0, "width": 393, "height": 54}, "children": [
   {"type": "StaticText", "label": "9:41", "isVisible": "1", "isAccessible": "1", "rect": {"x": 50, "y": 19, "width": 40, "height": 21}}
  ]},
  {"type": "NavigationBar", "name": "General", "isVisible": "1", "isAccessible": "0", "rect": {"x": 0, "y": 53, "width": 393, "height": 45}, "children": [
   {"type": "Button", "label": "Settings", "name": "Settings", "isEnabled": "1", "isVisible": "1", "isAccessible": "1", "traits": "Button", "rect": {"x": 0, "y": 53, "width": 94, "height": 45}},
   {"type": "StaticText", "label": "General", "isVisible": "1", "isAccessible": "1", "traits": "Header", "rect": {"x": 165, "y": 65, "width": 63, "height": 21}}
  ]},
  {"type": "SearchField", "label": "Search", "name": "Search", "value": "Search", "placeholderValue": "Search", "isEnabled": "1", "isVisible": "1", "isAccessible": "0", "isFocused": "1", "rect": {"x": 16, "y": 100, "width": 361, "height": 36}},
  {"type": "Cell", "isVisible": "1", "isAccessible": "0", "isEnabled": "1", "rect": {"x": 20, "y": 140, "width": 353, "height": 45}, "children": [
   {"type": "Button", "label": "About", "name": "com.apple.settings.about", "rawIdentifier": "com.apple.settings.about", "isEnabled": "1", "isVisible": "1", "isAccessible": "1", "traits": "Button", "rect": {"x": 20, "y": 140, "width": 353, "height": 45}, "children": [
    {"type": "StaticText", "label": "About", "value": "About", "isVisible": "1", "isAccessible": "0", "rect": {"x": 39, "y": 148, "width": 60, "height": 29}},
    {"type": "Image", "name": "chevron.forward", "isVisible": "1", "isAccessible": "0", "rect": {"x": 345, "y": 156, "width": 8, "height": 13}}
   ]}
  ]},
  {"type": "Cell", "isVisible": "1", "isAccessible": "0", "rect": {"x": 20, "y": 190, "width": 353, "height": 45}, "children": [
   {"type": "Switch", "label": "Airplane Mode", "value": "0", "isEnabled": "1", "isVisible": "1", "isAccessible": "1", "rect": {"x": 20, "y": 190, "width": 353, "height": 45}}
  ]},
  {"type": "Cell", "isVisible": "0", "isAccessible": "1", "rect": {"x": 20, "y": 900, "width": 353, "height": 45}, "children": [
   {"type": "Button", "label": "Below the fold", "isVisible": "0", "isAccessible": "1", "rect": {"x": 20, "y": 900, "width": 353, "height": 45}}
  ]},
  {"type": "Keyboard", "isVisible": "1", "isAccessible": "0", "rect": {"x": 0, "y": 583, "width": 393, "height": 233}, "children": [
   {"type": "Key", "label": "q", "isVisible": "1", "isAccessible": "1", "rect": {"x": 4, "y": 590, "width": 40, "height": 54}},
   {"type": "Key", "label": "w", "isVisible": "1", "isAccessible": "1", "rect": {"x": 44, "y": 590, "width": 40, "height": 54}}
  ]},
  {"type": "TabBar", "isVisible": "1", "rect": {"x": 0, "y": 790, "width": 393, "height": 62}, "children": [
   {"type": "Button", "label": "Favorites", "isVisible": "1", "isAccessible": "1", "isSelected": "1", "traits": "Button, Selected", "rect": {"x": 0, "y": 790, "width": 98, "height": 62}}
  ]}
 ]
}`

func settingsTree(t *testing.T) *wdaNode {
	t.Helper()
	var n wdaNode
	if err := json.Unmarshal([]byte(wdaSettingsTree), &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

func TestWDAScreenElements(t *testing.T) {
	els, kb := wdaScreenElements(settingsTree(t), 3, 1179, 2556, "com.apple.Preferences")
	if !kb {
		t.Error("keyboard not seen")
	}
	type want struct {
		role, label string
		bounds      screen.Rect
		states      []string
	}
	wants := []want{
		{screen.RoleButton, "Settings", screen.Rect{X: 0, Y: 159, W: 282, H: 135}, nil},
		{screen.RoleText, "General", screen.Rect{X: 495, Y: 195, W: 189, H: 63}, nil},
		{screen.RoleInput, "Search", screen.Rect{X: 48, Y: 300, W: 1083, H: 108}, []string{screen.StateEditable, screen.StateFocused}},
		{screen.RoleButton, "About", screen.Rect{X: 60, Y: 420, W: 1059, H: 135}, nil},
		{screen.RoleSwitch, "Airplane Mode", screen.Rect{X: 60, Y: 570, W: 1059, H: 135}, nil},
		{screen.RoleTab, "Favorites", screen.Rect{X: 0, Y: 2370, W: 294, H: 186}, []string{screen.StateSelected}},
	}
	if len(els) != len(wants) {
		var got []string
		for _, e := range els {
			got = append(got, e.Role+":"+e.Label)
		}
		t.Fatalf("got %d elements %v, want %d", len(els), got, len(wants))
	}
	for i, w := range wants {
		e := els[i]
		if e.Role != w.role || !strings.HasPrefix(e.Label, w.label) || e.Bounds != w.bounds || fmt.Sprint(e.States) != fmt.Sprint(w.states) {
			t.Errorf("element %d = %+v, want %+v", i, e, w)
		}
		if e.Source != screen.SourceTree || !strings.HasPrefix(e.Path, "com.apple.Preferences") {
			t.Errorf("element %d source %q path %q", i, e.Source, e.Path)
		}
	}
	if els[3].ID != "com.apple.settings.about" || !strings.Contains(els[3].Path, "/t75") {
		t.Errorf("About: id %q path %q", els[3].ID, els[3].Path)
	}
}

func TestWDABool(t *testing.T) {
	var v struct{ A, B, C, D, E wdaBool }
	if err := json.Unmarshal([]byte(`{"A":"1","B":"0","C":true,"D":1,"E":null}`), &v); err != nil {
		t.Fatal(err)
	}
	if !v.A || v.B || !v.C || !v.D || v.E {
		t.Fatalf("%+v", v)
	}
	if got := wdaTraits("Button, Selected, UpdatesFrequently"); got != traitButton|traitSelected {
		t.Fatalf("traits %b", got)
	}
	for in, want := range map[any]string{nil: "", "x": "x", 3.0: "3", 0.5: "0.5", true: "1"} {
		if got := wdaValue(in); got != want {
			t.Errorf("wdaValue(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestBackButton(t *testing.T) {
	p, ok := backButton(settingsTree(t), 3)
	if !ok || p != (screen.Point{X: 141, Y: 227}) {
		t.Fatalf("back button %v %v", p, ok)
	}
	var none wdaNode
	json.Unmarshal([]byte(`{"type":"Application","rect":{"width":393,"height":852},"children":[{"type":"Button","label":"x","isVisible":"1","rect":{"x":0,"y":0,"width":10,"height":10}}]}`), &none)
	if _, ok := backButton(&none, 3); ok {
		t.Fatal("found a back button without a navigation bar")
	}
}

func TestWDABind(t *testing.T) {
	entry := func(conn string) goios.DeviceEntry {
		var e goios.DeviceEntry
		e.Properties.ConnectionType = conn
		return e
	}
	tun := &Tunnel{device: netip.MustParseAddr("fd10:2c3d::1")}
	for _, c := range []struct {
		conn string
		t    *Tunnel
		want string
	}{
		{"USB", tun, "127.0.0.1"},
		{"Network", tun, "fd10:2c3d::1"},
		{"Network", nil, "127.0.0.1"},
	} {
		if got := wdaBind(entry(c.conn), c.t); got != c.want {
			t.Errorf("wdaBind(%s, tunnel %v) = %q, want %q", c.conn, c.t != nil, got, c.want)
		}
	}
	if got := wdaEnv("fd10:2c3d::1")["USE_IP"]; got != "fd10:2c3d::1" {
		t.Errorf("USE_IP = %v", got)
	}
}

func TestWDAGone(t *testing.T) {
	refused := func(context.Context, int) (net.Conn, error) { return nil, errors.New("connection was refused") }
	// Starting: WDA not answering yet is no reason to drop the phone.
	c := newWDADial(refused)
	if _, err := c.status(context.Background()); err == nil || errors.Is(err, driver.ErrOffline) {
		t.Fatalf("before open: %v", err)
	}
	// A runner Pinky did not start: nothing listens on WDA's port any more.
	d := &WDADevice{c: c}
	c.gone = d.gone
	if _, err := c.status(context.Background()); !errors.Is(err, driver.ErrOffline) {
		t.Fatalf("refused, runner not Pinky's: %v, want ErrOffline", err)
	}
	// A request that reached WDA and failed keeps the phone.
	other := errors.New("WebDriverAgent /source: EOF")
	if err := d.gone(other); errors.Is(err, driver.ErrOffline) {
		t.Fatalf("other error: %v", err)
	}
	// Pinky's runner, still running: a refused connection is only that.
	d.run = &wdaRun{done: make(chan struct{})}
	if _, err := c.status(context.Background()); err == nil || errors.Is(err, driver.ErrOffline) {
		t.Fatalf("refused, run going: %v, want an error that is not ErrOffline", err)
	}
	d.entry.DeviceID = 1 // a phone: the lock state comes from lockdown
	d.lockdownFn = func(context.Context) (bool, error) { return false, nil }
	if _, err := d.Locked(context.Background()); err != nil {
		t.Fatalf("lock state, run going: %v", err)
	}
	// Its test ended: any failed request, and the lock state the gateway
	// polls, which lockdown would still answer.
	close(d.run.done)
	if err := d.gone(other); !errors.Is(err, driver.ErrOffline) {
		t.Fatalf("run ended: %v, want ErrOffline", err)
	}
	if _, err := d.Locked(context.Background()); !errors.Is(err, driver.ErrOffline) {
		t.Fatalf("lock state, run ended: %v, want ErrOffline", err)
	}
}

func TestPickWDARunner(t *testing.T) {
	app := func(id, name string) installationproxy.AppInfo {
		return installationproxy.AppInfo{"CFBundleIdentifier": id, "CFBundleName": name, "CFBundleExecutable": name}
	}
	if got := pickWDARunner([]installationproxy.AppInfo{app("com.example.app", "App"), app("com.x.UITests.xctrunner", "UITests-Runner")}); got != "" {
		t.Errorf("picked %q from apps without WDA", got)
	}
	got := pickWDARunner([]installationproxy.AppInfo{
		app("com.facebook.WebDriverAgentRunner.xctrunner", "WebDriverAgentRunner-Runner"),
		app("cc.diygod.pinky.WebDriverAgentRunner.xctrunner", "WebDriverAgentRunner-Runner"),
	})
	if got != "cc.diygod.pinky.WebDriverAgentRunner.xctrunner" {
		t.Errorf("picked %q, want Pinky's build", got)
	}
	if got := pickWDARunner([]installationproxy.AppInfo{app("com.facebook.WebDriverAgentRunner.xctrunner", "WebDriverAgentRunner-Runner")}); got != "com.facebook.WebDriverAgentRunner.xctrunner" {
		t.Errorf("picked %q", got)
	}
}

func TestTouchRecorder(t *testing.T) {
	var r touchRecorder
	t0 := time.Unix(100, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	// A tap that wanders a little.
	r.touch(driver.TouchDown, screen.Point{X: 100, Y: 100}, at(0))
	r.touch(driver.TouchMove, screen.Point{X: 105, Y: 102}, at(30))
	g, err := r.touch(driver.TouchUp, screen.Point{X: 106, Y: 103}, at(80))
	if err != nil || g == nil || !g.tap || g.pts[0] != (screen.Point{X: 100, Y: 100}) || g.held != 80*time.Millisecond {
		t.Fatalf("tap: %+v %v", g, err)
	}
	// A drag: moves too close or too soon are dropped, the timing kept.
	r.touch(driver.TouchDown, screen.Point{X: 100, Y: 1000}, at(1000))
	r.touch(driver.TouchMove, screen.Point{X: 100, Y: 999}, at(1020)) // too close
	r.touch(driver.TouchMove, screen.Point{X: 100, Y: 900}, at(1030)) // kept
	r.touch(driver.TouchMove, screen.Point{X: 100, Y: 800}, at(1035)) // too soon
	r.touch(driver.TouchMove, screen.Point{X: 100, Y: 700}, at(1100)) // kept
	g, err = r.touch(driver.TouchUp, screen.Point{X: 100, Y: 600}, at(1200))
	if err != nil || g == nil || g.tap {
		t.Fatalf("drag: %+v %v", g, err)
	}
	wantPts := []screen.Point{{X: 100, Y: 1000}, {X: 100, Y: 900}, {X: 100, Y: 700}, {X: 100, Y: 600}}
	wantSteps := []time.Duration{0, 30 * time.Millisecond, 70 * time.Millisecond, 100 * time.Millisecond}
	if fmt.Sprint(g.pts) != fmt.Sprint(wantPts) || fmt.Sprint(g.steps) != fmt.Sprint(wantSteps) {
		t.Fatalf("drag path %v steps %v", g.pts, g.steps)
	}
	// A move or an up without a down does nothing.
	if g, _ := r.touch(driver.TouchUp, screen.Point{}, at(2000)); g != nil {
		t.Fatal("up without down")
	}
	// A long path keeps its ends.
	r.touch(driver.TouchDown, screen.Point{X: 0, Y: 0}, at(3000))
	for i := 1; i <= 500; i++ {
		r.touch(driver.TouchMove, screen.Point{X: i * 5, Y: 0}, at(3000+i*20))
	}
	g, _ = r.touch(driver.TouchUp, screen.Point{X: 2600, Y: 0}, at(14000))
	if len(g.pts) > recMaxPoints+1 || g.pts[0] != (screen.Point{}) || g.pts[len(g.pts)-1] != (screen.Point{X: 2600}) {
		t.Fatalf("long path: %d points, %v .. %v", len(g.pts), g.pts[0], g.pts[len(g.pts)-1])
	}
	var total time.Duration
	for _, s := range g.steps {
		total += s
	}
	if total != 11*time.Second {
		t.Fatalf("long path lasts %v, want 11s", total)
	}
}

func jpegBytes(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestMJPEGReader(t *testing.T) {
	a, b := jpegBytes(t, 32, 64, color.White), jpegBytes(t, 32, 64, color.Black)
	var s bytes.Buffer
	// WDA's framing, then a part without Content-Length and blank lines.
	fmt.Fprintf(&s, "--BoundaryString\r\nContent-type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(a))
	s.Write(a)
	fmt.Fprintf(&s, "\r\n\r\n--BoundaryString\r\nContent-type: image/jpeg\r\n\r\n")
	s.Write(b)
	fmt.Fprintf(&s, "\r\n--BoundaryString\r\nContent-Length: %d\r\n\r\n", len(a))
	s.Write(a)
	r := newMJPEGReader(&s, nil)
	ctx := context.Background()
	var keys []uint64
	for i := 0; i < 3; i++ {
		f, err := r.next(ctx)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		img, err := f.decode()
		if err != nil || img.Bounds().Dx() != 32 || img.Bounds().Dy() != 64 {
			t.Fatalf("frame %d: %v %v", i, img, err)
		}
		keys = append(keys, f.key)
	}
	if keys[0] != keys[2] || keys[0] == keys[1] {
		t.Fatalf("keys %v: equal frames must share a key", keys)
	}
	if _, err := r.next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last frame: %v", err)
	}
}

func TestAVCCToAnnexB(t *testing.T) {
	avcc := []byte{0, 0, 0, 2, 0x65, 0xAA, 0, 0, 0, 3, 0x41, 0xBB, 0xCC}
	got := avccToAnnexB(avcc, 4)
	want := []byte{0, 0, 0, 1, 0x65, 0xAA, 0, 0, 0, 1, 0x41, 0xBB, 0xCC}
	if !bytes.Equal(got, want) {
		t.Fatalf("annex b % x", got)
	}
	if !hasIDR(got) || hasIDR(want[6:]) {
		t.Fatal("IDR detection")
	}
	if len(avccToAnnexB([]byte{0, 0, 0, 9, 1}, 4)) != 0 {
		t.Fatal("a truncated unit must be dropped")
	}
}

func TestToNV12(t *testing.T) {
	// JPEG's 4:2:0 pictures copy straight; odd sizes are cropped to even.
	img, err := jpeg.Decode(bytes.NewReader(jpegBytes(t, 33, 65, color.RGBA{200, 30, 60, 255})))
	if err != nil {
		t.Fatal(err)
	}
	y, uv, w, h := toNV12(img)
	if w != 32 || h != 64 || len(y) != 32*64 || len(uv) != 32*32 {
		t.Fatalf("size %dx%d, planes %d %d", w, h, len(y), len(uv))
	}
	yc := img.(*image.YCbCr)
	if y[0] != yc.Y[0] || uv[0] != yc.Cb[0] || uv[1] != yc.Cr[0] {
		t.Fatalf("planes not copied: %d %d %d", y[0], uv[0], uv[1])
	}
	// RGBA goes through BT.601: white is Y 235, gray chroma.
	rgba := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range rgba.Pix {
		rgba.Pix[i] = 255
	}
	y, uv, _, _ = toNV12(rgba)
	if y[0] < 233 || y[0] > 236 || uv[0] < 127 || uv[0] > 129 || uv[1] < 127 || uv[1] > 129 {
		t.Fatalf("white: Y %d, Cb %d, Cr %d", y[0], uv[0], uv[1])
	}
}

// fakeWDA is a WebDriverAgent that answers from a handler per path and
// records what it was sent.
type fakeWDA struct {
	mu       sync.Mutex
	sessions int
	bodies   map[string][]string
	expire   bool // the next session request fails as WDA does after a restart

	// For reads.
	app         string        // /wda/activeAppInfo's value
	locked      bool          // /wda/locked
	sourceDelay time.Duration // before /source answers; set before the first request
	shots       int           // /screenshot requests
	shotAt      time.Time     // the last one
	sourceAt    time.Time     // when /source last answered
	version     string        // /status's build version: "" is 16.17.6, "none" none
}

func (f *fakeWDA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/source") && f.sourceDelay > 0 {
		time.Sleep(f.sourceDelay) // a phone's tree takes long; other requests go on
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	if f.bodies == nil {
		f.bodies = map[string][]string{}
	}
	path := r.URL.Path
	if i := strings.Index(path, "/session/"); i == 0 {
		rest := strings.TrimPrefix(path, "/session/")
		if j := strings.IndexByte(rest, '/'); j > 0 {
			sid := rest[:j]
			path = rest[j:]
			if f.expire || sid != fmt.Sprintf("s%d", f.sessions) {
				f.expire = false
				w.WriteHeader(404)
				fmt.Fprint(w, `{"value":{"error":"invalid session id","message":"Session does not exist"}}`)
				return
			}
		}
	}
	f.bodies[r.Method+" "+path] = append(f.bodies[r.Method+" "+path], string(b))
	switch r.Method + " " + path {
	case "GET /status":
		v := f.version
		switch v {
		case "":
			v = "16.17.6"
		case "none":
			v = ""
		}
		fmt.Fprintf(w, `{"value":{"ready":true,"build":{"version":%q},"os":{"version":"18.5"}},"sessionId":null}`, v)
	case "POST /session":
		f.sessions++
		fmt.Fprintf(w, `{"value":{"sessionId":"s%d","capabilities":{}},"sessionId":"s%d"}`, f.sessions, f.sessions)
	case "POST /appium/settings", "POST /actions", "POST /wda/keys", "POST /element/E1/clear", "POST /url":
		fmt.Fprint(w, `{"value":null}`)
	case "GET /wda/activeAppInfo":
		fmt.Fprintf(w, `{"value":%s}`, f.app)
	case "GET /wda/locked":
		fmt.Fprintf(w, `{"value":%v}`, f.locked)
	case "GET /source":
		f.sourceAt = time.Now()
		fmt.Fprint(w, `{"value":{"type":"XCUIElementTypeApplication","label":"App","rect":{"x":0,"y":0,"width":100,"height":200},"isVisible":"1",`+
			`"children":[{"type":"XCUIElementTypeButton","label":"Pay","rect":{"x":10,"y":10,"width":50,"height":20},"isVisible":"1","isAccessible":"1"}]}}`)
	case "GET /screenshot":
		f.shots++
		f.shotAt = time.Now()
		w.WriteHeader(500) // the read goes on without OCR's text
		fmt.Fprint(w, `{"value":{"error":"unknown error","message":"no picture in tests"}}`)
	case "GET /element/active":
		if strings.Contains(r.URL.RawQuery, "none") {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"value":{"error":"no such element","message":"No element is focused"}}`)
			return
		}
		fmt.Fprint(w, `{"value":{"ELEMENT":"E1"}}`)
	default:
		w.WriteHeader(404)
		fmt.Fprintf(w, `{"value":{"error":"unknown command","message":"%s"}}`, path)
	}
}

func TestWDAClient(t *testing.T) {
	f := &fakeWDA{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := newWDAURL(srv.URL, "")
	ctx := context.Background()
	st, err := c.waitReady(ctx)
	if err != nil || !st.Ready || st.OS.Version != "18.5" {
		t.Fatalf("status %+v %v", st, err)
	}
	if err := c.gesture(ctx, []wdaPoint{{10, 20}}, 0, 60*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if f.sessions != 1 || len(f.bodies["POST /appium/settings"]) != 1 {
		t.Fatalf("sessions %d, settings %v", f.sessions, f.bodies["POST /appium/settings"])
	}
	var acts struct {
		Actions []struct {
			Type       string `json:"type"`
			Parameters struct {
				PointerType string `json:"pointerType"`
			} `json:"parameters"`
			Actions []map[string]any `json:"actions"`
		} `json:"actions"`
	}
	json.Unmarshal([]byte(f.bodies["POST /actions"][0]), &acts)
	a := acts.Actions[0]
	if a.Type != "pointer" || a.Parameters.PointerType != "touch" || len(a.Actions) != 4 ||
		a.Actions[0]["x"] != 10.0 || a.Actions[2]["type"] != "pause" || a.Actions[2]["duration"] != 60.0 || a.Actions[3]["type"] != "pointerUp" {
		t.Fatalf("tap actions %+v", a)
	}
	// WDA restarted: the session is gone, so a new one is made once.
	f.mu.Lock()
	f.expire = true
	f.mu.Unlock()
	if err := c.keys(ctx, "héllo 你好"); err != nil {
		t.Fatal(err)
	}
	if f.sessions != 2 || !strings.Contains(f.bodies["POST /wda/keys"][0], "你好") {
		t.Fatalf("sessions %d, keys %v", f.sessions, f.bodies["POST /wda/keys"])
	}
	if err := c.clearFocused(ctx); err != nil {
		t.Fatal(err)
	}
	// Errors come back typed.
	err = c.do(ctx, c.hc, http.MethodPost, "/wda/nothing", map[string]any{}, nil)
	var we *wdaError
	if !errors.As(err, &we) || we.Code != "unknown command" {
		t.Fatalf("error %v", err)
	}
	if err := c.do(ctx, c.hc, http.MethodGet, "/element/active?none", nil, nil); !errors.As(err, &we) || we.Code != "no such element" {
		t.Fatalf("no such element: %v", err)
	}
}

func TestProviderLegacyBackend(t *testing.T) {
	p := NewProvider()
	ok := true
	runner := ""
	p.legacySetup = func(context.Context, goios.DeviceEntry) (bool, error) { return ok, nil }
	p.findRunner = func(goios.DeviceEntry) (string, error) { return runner, nil }
	p.helperFn = func(context.Context, goios.DeviceEntry, wdaRunner) (time.Time, error) { return time.Time{}, nil }
	e := entry("old", 1, "USB")
	base := driver.Info{ID: "old", State: driver.StateOnline, Backend: backendCoreDevice, OSVersion: "18.6"}
	if got := p.inspectLegacy(context.Background(), e, base); got.State != driver.StateOnline || got.Backend != backendAccessibility {
		t.Fatalf("without the helper: %+v", got)
	}
	runner = "com.facebook.WebDriverAgentRunner.xctrunner"
	if got := p.inspectLegacy(context.Background(), e, base); got.Backend != backendWDA {
		t.Fatalf("with the helper: %+v", got)
	}
	p.ddi = map[string]ddiCheck{} // forget the cached check
	ok = false
	if got := p.inspectLegacy(context.Background(), e, base); got.State != driver.StateNeedsSetup || !strings.Contains(got.Detail, "developer disk image") {
		t.Fatalf("without the disk image: %+v", got)
	}
}

func TestProviderRechecksAccessibilityPhones(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	p := NewProvider()
	p.now = func() time.Time { return now }
	p.inspectFn = func(_ context.Context, e goios.DeviceEntry) driver.Info {
		calls++
		return driver.Info{ID: e.Properties.SerialNumber, State: driver.StateOnline, Backend: backendAccessibility}
	}
	e := entry("old", 1, "USB")
	p.info(context.Background(), e)
	now = now.Add(10 * time.Second)
	p.info(context.Background(), e)
	if calls != 1 {
		t.Fatalf("asked %d times within 10 s", calls)
	}
	now = now.Add(25 * time.Second)
	p.info(context.Background(), e)
	if calls != 2 {
		t.Fatalf("asked %d times after 35 s; a phone without the helper is asked again", calls)
	}
}

func TestFrameSettle(t *testing.T) {
	// Frames every 20 ms: changing for the first 300 ms, then still.
	run := func(changeFor time.Duration, limit time.Duration) (bool, time.Duration) {
		start := time.Now()
		n := uint64(0)
		settled, err := frameSettle(context.Background(), limit, func(ctx context.Context) (uint64, error) {
			if err := sleepCtx(ctx, 20*time.Millisecond); err != nil {
				return 0, err
			}
			if time.Since(start) < changeFor {
				n++
			}
			return n, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return settled, time.Since(start)
	}
	if ok, took := run(300*time.Millisecond, 3*time.Second); !ok || took < settleMin || took > 900*time.Millisecond {
		t.Errorf("settling after 300 ms of motion: %v after %v", ok, took)
	}
	if ok, took := run(time.Hour, 600*time.Millisecond); ok || took > time.Second {
		t.Errorf("endless motion: settled %v after %v, want false at the limit", ok, took)
	}
}

// An iPad's Settings in a split view, as WDA gave it on the iPad Simulator
// (iPadOS 26.5) on General › Keyboards: the sidebar's bar first, holding
// the search field and its Dictate button, then the page's bar with its
// back button past the screen's left third.
const wdaSplitViewTree = `{"type":"Application","label":"Settings","isVisible":"1","rect":{"x":0,"y":0,"width":834,"height":1210},"children":[
 {"type":"NavigationBar","name":"Settings","isVisible":"1","rect":{"x":10,"y":32,"width":320,"height":114},"children":[
  {"type":"SearchField","name":"Search","label":"Search","isVisible":"1","rect":{"x":26,"y":87,"width":288,"height":44},"children":[
   {"type":"Button","name":"Dictate","label":"Dictate","rawIdentifier":"Dictate","isVisible":"1","rect":{"x":281,"y":98,"width":17,"height":22}}]}]},
 {"type":"NavigationBar","name":"Keyboards","isVisible":"1","rect":{"x":0,"y":32,"width":834,"height":54},"children":[
  {"type":"Button","name":"BackButton","label":"General","rawIdentifier":"BackButton","isVisible":"1","rect":{"x":350,"y":32,"width":44,"height":44}},
  {"type":"StaticText","name":"Keyboards","label":"Keyboards","isVisible":"1","rect":{"x":544,"y":43,"width":86,"height":22}}]}]}`

func TestBackButtonSplitView(t *testing.T) {
	var root wdaNode
	if err := json.Unmarshal([]byte(wdaSplitViewTree), &root); err != nil {
		t.Fatal(err)
	}
	p, ok := backButton(&root, 2)
	if !ok || p != (screen.Point{X: 744, Y: 108}) {
		t.Fatalf("back button %v %v, want the page's at 744,108", p, ok)
	}
	// Without a BackButton, the sidebar's Dictate button is still not it
	// (it is in the search field): Back then swipes from the edge.
	root.Children[1].Children[0].Name, root.Children[1].Children[0].Identifier = nil, nil
	if p, ok = backButton(&root, 2); ok {
		t.Fatalf("leading button %v", p)
	}
}
