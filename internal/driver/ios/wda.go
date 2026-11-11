package ios

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
)

// WebDriverAgent (Appium's XCUITest server, BSD-licensed) is the helper app
// that drives iPhones below iOS 27, where the phone refuses CoreDevice's
// screen stream and HID input. It runs as a UI test on the phone, serves
// HTTP on port 8100 and an MJPEG picture on port 9100, and does everything
// through XCTest: touches at any point, typing, Home, launching apps, and
// the element tree with frames. Pinky reaches both ports through usbmuxd
// on a cable and through its tunnel on Wi-Fi (wdaBind), one connection per
// request, so nothing listens on the computer.

// WDA ports on the phone.
const (
	wdaPort   = 8100
	mjpegPort = 9100
)

// wdaClient is an HTTP client for one running WebDriverAgent.
type wdaClient struct {
	base  string // "http://wda" when dialing through usbmuxd, else the server's URL
	mjpeg string // the MJPEG server's URL
	hc    *http.Client
	// long serves requests that may take long on the phone (the tree of a
	// busy screen, a screenshot) and the MJPEG stream, which never ends.
	long *http.Client

	// gone, once the phone is open, maps a request that failed to reach WDA
	// (WDADevice.gone).
	gone func(error) error

	mu  sync.Mutex
	sid string // the WebDriver session
}

// wdaDialError is a connection to WDA's port on the phone that could not be
// opened: nothing listens there any more, or the way there is gone.
type wdaDialError struct{ err error }

func (e *wdaDialError) Error() string { return e.err.Error() }
func (e *wdaDialError) Unwrap() error { return e.err }

// newWDAUSB returns a client that reaches WDA on the phone behind a usbmuxd
// entry.
func newWDAUSB(e goios.DeviceEntry) *wdaClient {
	return newWDADial(func(ctx context.Context, port int) (net.Conn, error) {
		return dialDevicePort(ctx, e.DeviceID, port)
	})
}

// newWDATunnel returns a client that reaches WDA on the phone through a
// tunnel, at the phone's address inside it.
func newWDATunnel(t *Tunnel) *wdaClient {
	return newWDADial(func(ctx context.Context, port int) (net.Conn, error) {
		c, err := t.DialTCP(ctx, port)
		if err != nil {
			return nil, err
		}
		return c, nil
	})
}

// newWDADial returns a client that opens its connections to the phone's
// ports with dial.
func newWDADial(dial func(ctx context.Context, port int) (net.Conn, error)) *wdaClient {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			port := wdaPort
			if strings.HasPrefix(addr, "mjpeg:") {
				port = mjpegPort
			}
			c, err := dial(ctx, port)
			if err != nil && ctx.Err() == nil {
				return nil, &wdaDialError{err}
			}
			return c, err
		},
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
		TLSHandshakeTimeout: time.Second,
	}
	return &wdaClient{
		base:  "http://wda",
		mjpeg: "http://mjpeg/",
		hc:    &http.Client{Transport: tr, Timeout: 30 * time.Second},
		long:  &http.Client{Transport: tr},
	}
}

// newWDAURL returns a client for a WDA server at a URL, such as one running
// on the iOS Simulator (which shares the computer's network).
func newWDAURL(base, mjpeg string) *wdaClient {
	return &wdaClient{
		base:  strings.TrimSuffix(base, "/"),
		mjpeg: mjpeg,
		hc:    &http.Client{Timeout: 30 * time.Second},
		long:  &http.Client{},
	}
}

// dialDevicePort opens a TCP connection to a port on the phone through
// usbmuxd.
func dialDevicePort(ctx context.Context, deviceID, port int) (net.Conn, error) {
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		mux, err := goios.NewUsbMuxConnectionSimple()
		if err != nil {
			ch <- result{err: fmt.Errorf("usbmuxd: %w", err)}
			return
		}
		if err := mux.Connect(deviceID, uint16(port)); err != nil {
			mux.Close()
			ch <- result{err: fmt.Errorf("connect to port %d on the phone: %w", port, err)}
			return
		}
		ch <- result{c: mux.ReleaseDeviceConnection().Conn()}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// wdaError is an error WDA answered with.
type wdaError struct {
	Status  int
	Code    string // the WebDriver error code, such as "invalid session id"
	Message string
}

func (e *wdaError) Error() string {
	msg := e.Message
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	if e.Code != "" {
		return fmt.Sprintf("WebDriverAgent: %s: %s", e.Code, msg)
	}
	return fmt.Sprintf("WebDriverAgent: HTTP %d: %s", e.Status, msg)
}

func isSessionError(err error) bool {
	var we *wdaError
	return errors.As(err, &we) && (we.Code == "invalid session id" || strings.Contains(strings.ToLower(we.Message), "session does not exist"))
}

// call sends one request. A nil body sends no body; out, when not nil,
// receives the answer's "value".
func (c *wdaClient) call(ctx context.Context, hc *http.Client, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		err = fmt.Errorf("WebDriverAgent %s: %w", path, err)
		if c.gone != nil {
			err = c.gone(err)
		}
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("WebDriverAgent %s: %w", path, err)
	}
	var env struct {
		Value     json.RawMessage `json:"value"`
		SessionID string          `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("WebDriverAgent %s: HTTP %d, not JSON: %.120q", path, resp.StatusCode, raw)
	}
	if resp.StatusCode >= 300 || hasWDAError(env.Value) {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		json.Unmarshal(env.Value, &e)
		return &wdaError{Status: resp.StatusCode, Code: e.Error, Message: e.Message}
	}
	if out != nil && len(env.Value) > 0 {
		if err := json.Unmarshal(env.Value, out); err != nil {
			return fmt.Errorf("WebDriverAgent %s: %w", path, err)
		}
	}
	return nil
}

func hasWDAError(v json.RawMessage) bool {
	if len(v) == 0 || v[0] != '{' {
		return false
	}
	var e struct {
		Error string `json:"error"`
	}
	return json.Unmarshal(v, &e) == nil && e.Error != ""
}

// wdaStatus is what /status says.
type wdaStatus struct {
	Ready   bool   `json:"ready"`
	Message string `json:"message"`
	Build   struct {
		Version string `json:"version"`
	} `json:"build"`
	OS struct {
		Version string `json:"version"`
	} `json:"os"`
}

func (c *wdaClient) status(ctx context.Context) (wdaStatus, error) {
	var s wdaStatus
	err := c.call(ctx, c.hc, http.MethodGet, "/status", nil, &s)
	return s, err
}

// waitReady polls /status until WDA answers, for a runner that is starting.
func (c *wdaClient) waitReady(ctx context.Context) (wdaStatus, error) {
	var last error
	for {
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		s, err := c.status(sctx)
		cancel()
		if err == nil && s.Ready {
			return s, nil
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			if last == nil {
				last = ctx.Err()
			}
			return s, fmt.Errorf("WebDriverAgent did not start: %w", last)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// wdaSettings are the session settings Pinky uses. Snapshots stop at a
// depth that covers real apps; the MJPEG picture is the screen at half size,
// enough for the mirror and light for the encoder.
var wdaSettings = map[string]any{
	"snapshotMaxDepth":             60,
	"mjpegServerFramerate":         15,
	"mjpegServerScreenshotQuality": 50,
	"mjpegScalingFactor":           50,
	"shouldUseCompactResponses":    true,
	"keyboardAutocorrection":       false,
	"keyboardPrediction":           false,
	// Wait less for the app to idle before reading the tree: Pinky settles
	// by itself, and some apps never idle (animations).
	"waitForIdleTimeout":      1,
	"animationCoolOffTimeout": 0,
}

// session returns the WebDriver session, creating it once.
func (c *wdaClient) session(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sid != "" {
		return c.sid, nil
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	body := map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{
		// Never relaunch or kill the app in front.
		"shouldWaitForQuiescence": false,
	}}}
	if err := c.call(ctx, c.hc, http.MethodPost, "/session", body, &out); err != nil {
		return "", err
	}
	if out.SessionID == "" {
		return "", errors.New("WebDriverAgent: no session id")
	}
	c.sid = out.SessionID
	if err := c.call(ctx, c.hc, http.MethodPost, "/session/"+c.sid+"/appium/settings", map[string]any{"settings": wdaSettings}, nil); err != nil {
		return "", err
	}
	return c.sid, nil
}

// do runs a session request, starting a new session once when WDA lost its
// own (it restarted).
func (c *wdaClient) do(ctx context.Context, hc *http.Client, method, path string, body, out any) error {
	for attempt := 0; ; attempt++ {
		sid, err := c.session(ctx)
		if err != nil {
			return err
		}
		err = c.call(ctx, hc, method, "/session/"+sid+path, body, out)
		if err == nil || attempt > 0 || !isSessionError(err) {
			return err
		}
		c.mu.Lock()
		if c.sid == sid {
			c.sid = ""
		}
		c.mu.Unlock()
	}
}

// wdaScreen is /wda/screen: the screen in points and its scale.
type wdaScreen struct {
	Scale      float64 `json:"scale"`
	ScreenSize struct {
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	} `json:"screenSize"`
}

func (c *wdaClient) screenInfo(ctx context.Context) (wdaScreen, error) {
	var s wdaScreen
	if err := c.do(ctx, c.hc, http.MethodGet, "/wda/screen", nil, &s); err != nil {
		return s, err
	}
	if s.Scale <= 0 || s.ScreenSize.Width <= 0 {
		var ws struct {
			Width  float64 `json:"width"`
			Height float64 `json:"height"`
		}
		if err := c.do(ctx, c.hc, http.MethodGet, "/window/size", nil, &ws); err != nil {
			return s, err
		}
		s.ScreenSize.Width, s.ScreenSize.Height = ws.Width, ws.Height
		if s.Scale <= 0 {
			s.Scale = 1
		}
	}
	return s, nil
}

// wdaNode is one element of WDA's JSON page source.
type wdaNode struct {
	Type        string    `json:"type"`
	Label       *string   `json:"label"`
	Name        *string   `json:"name"`
	Value       any       `json:"value"`
	Identifier  *string   `json:"rawIdentifier"`
	Rect        wdaRect   `json:"rect"`
	Enabled     *wdaBool  `json:"isEnabled"` // nil: not said, so enabled
	Visible     wdaBool   `json:"isVisible"`
	Accessible  wdaBool   `json:"isAccessible"`
	Focused     wdaBool   `json:"isFocused"`
	Selected    wdaBool   `json:"isSelected"`
	Traits      string    `json:"traits"`
	Placeholder *string   `json:"placeholderValue"`
	Children    []wdaNode `json:"children"`
}

type wdaRect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// wdaBool reads WDA's booleans, which come as "1"/"0" strings, numbers or
// JSON booleans depending on the version.
type wdaBool bool

func (b *wdaBool) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	*b = wdaBool(s == "1" || s == "true")
	return nil
}

func (c *wdaClient) source(ctx context.Context) (*wdaNode, error) {
	var root wdaNode
	// The tree of a busy screen takes seconds on a phone.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.do(cctx, c.long, http.MethodGet, "/source?format=json", nil, &root); err != nil {
		return nil, err
	}
	return &root, nil
}

func (c *wdaClient) screenshot(ctx context.Context) ([]byte, error) {
	var b64 string
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.do(cctx, c.long, http.MethodGet, "/screenshot", nil, &b64); err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("WebDriverAgent screenshot: %w", err)
	}
	return b, nil
}

// wdaPoint is a point in screen points, which WDA's gestures take.
type wdaPoint struct{ X, Y float64 }

// gesture performs one finger's path as W3C pointer actions: down at the
// first point, a move to each next one taking step, and up at the last. A
// one-point path is a tap held for hold.
func (c *wdaClient) gesture(ctx context.Context, path []wdaPoint, step, hold time.Duration) error {
	steps := make([]time.Duration, len(path))
	for i := 1; i < len(steps); i++ {
		steps[i] = step
	}
	return c.gesturePath(ctx, path, steps, hold)
}

// gesturePath is gesture with a duration for each move: steps[i] is how
// long the move to path[i] takes (steps[0] is ignored).
func (c *wdaClient) gesturePath(ctx context.Context, path []wdaPoint, steps []time.Duration, hold time.Duration) error {
	if len(path) == 0 || len(steps) != len(path) {
		return errors.New("empty gesture")
	}
	acts := []map[string]any{
		{"type": "pointerMove", "duration": 0, "x": path[0].X, "y": path[0].Y},
		{"type": "pointerDown", "button": 0},
	}
	if len(path) == 1 {
		acts = append(acts, map[string]any{"type": "pause", "duration": hold.Milliseconds()})
	}
	for i, p := range path[1:] {
		acts = append(acts, map[string]any{"type": "pointerMove", "duration": max(steps[i+1].Milliseconds(), 1), "x": p.X, "y": p.Y})
	}
	acts = append(acts, map[string]any{"type": "pointerUp", "button": 0})
	body := map[string]any{"actions": []any{map[string]any{
		"type": "pointer", "id": "finger1",
		"parameters": map[string]any{"pointerType": "touch"},
		"actions":    acts,
	}}}
	return c.do(ctx, c.long, http.MethodPost, "/actions", body, nil)
}

func (c *wdaClient) keys(ctx context.Context, text string) error {
	return c.do(ctx, c.long, http.MethodPost, "/wda/keys", map[string]any{"value": []string{text}}, nil)
}

// clearFocused empties the text field that has the keyboard focus (the
// active element, in WebDriver's terms). It returns errNoFocus when no
// field has it.
func (c *wdaClient) clearFocused(ctx context.Context) error {
	var el map[string]string
	err := c.do(ctx, c.hc, http.MethodGet, "/element/active", nil, &el)
	if err != nil {
		var we *wdaError
		if errors.As(err, &we) && we.Code == "no such element" {
			return fmt.Errorf("no text field has the keyboard focus: %w", errNoFocus)
		}
		return err
	}
	id := el["ELEMENT"]
	if id == "" {
		id = el["element-6066-11e4-a52e-4f735466cecf"]
	}
	if id == "" {
		return fmt.Errorf("no text field has the keyboard focus: %w", errNoFocus)
	}
	return c.do(ctx, c.long, http.MethodPost, "/element/"+id+"/clear", map[string]any{}, nil)
}

var errNoFocus = errors.New("no focused field")

func (c *wdaClient) home(ctx context.Context) error {
	return c.call(ctx, c.hc, http.MethodPost, "/wda/homescreen", map[string]any{}, nil)
}

// pressHome presses the Home button and nothing more. /wda/homescreen
// also brings SpringBoard to the front, which iOS refuses while the phone
// is locked ("Unable to launch … because the device was not, or could not
// be, unlocked"); on the lock screen the press alone wakes the screen.
func (c *wdaClient) pressHome(ctx context.Context) error {
	return c.do(ctx, c.hc, http.MethodPost, "/wda/pressButton", map[string]any{"name": "home"}, nil)
}

// activate brings an app to the front, launching it when it is not
// running; unlike launch it never restarts a running app.
func (c *wdaClient) activate(ctx context.Context, bundleID string) error {
	return c.do(ctx, c.long, http.MethodPost, "/wda/apps/activate", map[string]any{"bundleId": bundleID}, nil)
}

// terminate ends an app, as XCTest's terminate does; one not running is
// left as it is.
func (c *wdaClient) terminate(ctx context.Context, bundleID string) error {
	return c.do(ctx, c.long, http.MethodPost, "/wda/apps/terminate", map[string]any{"bundleId": bundleID}, nil)
}

type wdaApp struct {
	BundleID string `json:"bundleId"`
	Name     string `json:"name"`
	PID      int    `json:"pid"`
}

func (c *wdaClient) activeApp(ctx context.Context) (wdaApp, error) {
	var a wdaApp
	err := c.call(ctx, c.hc, http.MethodGet, "/wda/activeAppInfo", nil, &a)
	return a, err
}

// rotation is GET /rotation: the active app's interface orientation as a
// turn around z, 0, 90, 180 or 270 degrees, clockwise from portrait. Unlike
// GET /orientation, which WebDriver folds into portrait and landscape, it
// tells upside down apart.
func (c *wdaClient) rotation(ctx context.Context) (int, error) {
	var r struct {
		Z float64 `json:"z"`
	}
	err := c.call(ctx, c.hc, http.MethodGet, "/rotation", nil, &r)
	return int(math.Round(r.Z)), err
}

func (c *wdaClient) locked(ctx context.Context) (bool, error) {
	var b bool
	err := c.call(ctx, c.hc, http.MethodGet, "/wda/locked", nil, &b)
	return b, err
}

// deleteSession ends the WebDriver session, best effort.
func (c *wdaClient) deleteSession(ctx context.Context) {
	c.mu.Lock()
	sid := c.sid
	c.sid = ""
	c.mu.Unlock()
	if sid != "" {
		c.call(ctx, c.hc, http.MethodDelete, "/session/"+sid, nil, nil)
	}
}
