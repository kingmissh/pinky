package main

import (
	"context"
	"image"
	"testing"
	"time"

	"github.com/DIYgod/pinky/internal/driver"
	"github.com/DIYgod/pinky/internal/driver/ios"
	"github.com/DIYgod/pinky/internal/screen"
)

// plainDevice is a phone that is not a WebDriverAgent one, such as an
// Android phone: it always takes raw touches.
type plainDevice struct{}

func (plainDevice) Info() driver.Info { return driver.Info{} }

func (plainDevice) Observe(context.Context, driver.ObserveOptions) (*screen.Screen, error) {
	return nil, nil
}

func (plainDevice) Screenshot(context.Context) (image.Image, error) { return nil, nil }

func (plainDevice) Tap(context.Context, screen.Point, driver.TapOptions) error { return nil }

func (plainDevice) Swipe(context.Context, screen.Point, screen.Point, time.Duration) error {
	return nil
}

func (plainDevice) Type(context.Context, string, driver.TypeOptions) error { return nil }

func (plainDevice) Key(context.Context, driver.Key) error { return nil }

func (plainDevice) Launch(context.Context, string) error { return nil }

func (plainDevice) Apps(context.Context) ([]driver.App, error) { return nil, nil }

func (plainDevice) Stream(context.Context, driver.StreamOptions) (driver.Stream, error) {
	return nil, nil
}

func (plainDevice) Close() error { return nil }

var _ driver.Device = plainDevice{}

func TestUseSwipe(t *testing.T) {
	// A WebDriverAgent device, whose raw touches are replayed when the
	// finger lifts, swipes unless -raw-touch asks for the raw touches; any
	// other device takes raw touches either way.
	cases := []struct {
		name string
		dev  driver.Device
		raw  bool
		want bool
	}{
		{"wda without -raw-touch", &ios.WDADevice{}, false, true},
		{"wda with -raw-touch", &ios.WDADevice{}, true, false},
		{"other device without -raw-touch", plainDevice{}, false, false},
		{"other device with -raw-touch", plainDevice{}, true, false},
	}
	for _, c := range cases {
		if got := useSwipe(c.dev, c.raw); got != c.want {
			t.Errorf("%s: useSwipe = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTouchRoute(t *testing.T) {
	cases := []struct {
		name string
		dev  driver.Device
		raw  bool
		want string
	}{
		{"wda without -raw-touch", &ios.WDADevice{}, false, "wda swipe"},
		{"wda with -raw-touch", &ios.WDADevice{}, true, "raw touch"},
		{"other device without -raw-touch", plainDevice{}, false, "raw touch"},
		{"other device with -raw-touch", plainDevice{}, true, "raw touch"},
	}
	for _, c := range cases {
		if got := touchRoute(c.dev, c.raw); got != c.want {
			t.Errorf("%s: touchRoute = %q, want %q", c.name, got, c.want)
		}
	}
}
